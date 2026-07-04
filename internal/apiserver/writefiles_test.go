package apiserver

import (
	"bytes"
	"context"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// writeFilesSvc returns a fake-backed service, its wired container FS, and a
// created sandbox id.
func writeFilesSvc(t *testing.T) (*SandboxService, *fakeContainerFS, string) {
	t.Helper()
	svc := newSandboxSvc(t)
	fake := svc.mgr.Backend().(*sandbox.Fake)
	fs := newFakeContainerFS()
	fs.wire(fake)
	rec, err := svc.mgr.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)
	return svc, fs, rec.ID
}

// TestWriteFiles_LandsTreeWithModes writes a small Opencode-harness-like tree
// (relative + absolute + nested paths, with and without a mode) and asserts each
// file landed, the nested dir was created, chmod ran for mode-bearing files only,
// and Activity was bumped.
func TestWriteFiles_LandsTreeWithModes(t *testing.T) {
	svc, fs, id := writeFilesSvc(t)
	ctx := context.Background()
	before, _ := svc.mgr.Get(ctx, id)

	resp, err := svc.WriteFiles(ctx, &sbxv1.WriteFilesRequest{
		Id: id,
		Files: []*sbxv1.FileWrite{
			{Path: "opencode.json", Content: []byte(`{"model":"x"}`)},                   // relative → default dir, no mode
			{Path: ".opencode/agent/build.md", Content: []byte("# build"), Mode: 0o644}, // nested
			{Path: "/home/agent/run.sh", Content: []byte("echo hi"), Mode: 0o755},       // absolute + exec bit
		},
	})
	require.NoError(t, err)
	require.EqualValues(t, 3, resp.FilesWritten)

	got, ok := fs.get("/home/agent/opencode.json")
	require.True(t, ok, "relative path landed under the default upload dir")
	require.Equal(t, `{"model":"x"}`, string(got))

	got, ok = fs.get("/home/agent/.opencode/agent/build.md")
	require.True(t, ok, "nested path landed")
	require.Equal(t, "# build", string(got))
	require.True(t, fs.ranExec("mkdir", "-p", "/home/agent/.opencode/agent"), "nested dir created before write")

	got, ok = fs.get("/home/agent/run.sh")
	require.True(t, ok)
	require.Equal(t, "echo hi", string(got))

	require.True(t, fs.ranExec("chmod", "644", "/home/agent/.opencode/agent/build.md"), "mode applied as octal")
	require.True(t, fs.ranExec("chmod", "755", "/home/agent/run.sh"), "mode applied as octal")
	require.False(t, fs.ranExec("chmod", "0", "/home/agent/opencode.json"), "no chmod for mode 0")

	after, _ := svc.mgr.Get(ctx, id)
	require.True(t, after.LastActivity.After(before.LastActivity), "WriteFiles bumps Activity")
}

// TestWriteFiles_RejectsTraversal rejects a "../" path with InvalidArgument and
// writes nothing.
func TestWriteFiles_RejectsTraversal(t *testing.T) {
	svc, fs, id := writeFilesSvc(t)
	_, err := svc.WriteFiles(context.Background(), &sbxv1.WriteFilesRequest{
		Id:    id,
		Files: []*sbxv1.FileWrite{{Path: "../../etc/passwd", Content: []byte("pwned")}},
	})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, ok := fs.get("/etc/passwd")
	require.False(t, ok, "traversal target is never written")
}

// TestWriteFiles_UnknownSandbox maps a missing sandbox to NotFound.
func TestWriteFiles_UnknownSandbox(t *testing.T) {
	svc, _, _ := writeFilesSvc(t)
	_, err := svc.WriteFiles(context.Background(), &sbxv1.WriteFilesRequest{
		Id:    "n1.does-not-exist",
		Files: []*sbxv1.FileWrite{{Path: "a.txt", Content: []byte("x")}},
	})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestWriteFiles_LargeFileRoundTripsViaExec is the truncation-bug guard: a file
// well over the ~128 KiB daemon-truncation threshold must land byte-exact via the
// chunked transport. It writes the file, reads it back through the Exec RPC (cat),
// and asserts the bytes match and that the write spanned more than one append
// batch (i.e. it really chunked).
func TestWriteFiles_LargeFileRoundTripsViaExec(t *testing.T) {
	svc, fs, id := writeFilesSvc(t)
	ctx := context.Background()

	big := make([]byte, 1<<20) // 1 MiB, well past the 128 KiB truncation threshold
	for i := range big {
		big[i] = byte(i % 251) // varied so a chunk-boundary bug would corrupt, not hide
	}

	resp, err := svc.WriteFiles(ctx, &sbxv1.WriteFilesRequest{
		Id:    id,
		Files: []*sbxv1.FileWrite{{Path: "big.bin", Content: big}},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, resp.FilesWritten)
	require.Greater(t, fs.appendCalls, 1, "1 MiB spans more than one append batch")

	res, err := svc.Exec(ctx, &sbxv1.ExecRequest{Id: id, Cmd: []string{"cat", "/home/agent/big.bin"}})
	require.NoError(t, err)
	require.EqualValues(t, 0, res.ExitCode)
	require.True(t, bytes.Equal(big, res.Stdout), "large file round-trips byte-exact")
}
