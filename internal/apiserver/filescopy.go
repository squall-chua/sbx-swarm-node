package apiserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// The sbx daemon's file transport (sbx cp and exec stdin/stdout streaming)
// intermittently truncates transfers larger than ~128 KiB ("tar: Unexpected
// EOF"); the failure rate is high and not size-correlated, so retrying a whole
// large transfer does not converge. Instead we move the bytes over the daemon's
// *request* path, which is reliable: upload base64-encodes the file into exec
// argv chunks that the sandbox decodes and appends; download reads it back in
// small exec-stdout slices. Each chunk is small enough to be reliable, and is
// size-verified with a bounded retry.
const (
	transferTries  = 8
	execChunkRaw   = 90000 // raw bytes per chunk: a multiple of 3 (clean base64) whose
	execChunkBatch = 8     // base64 (120000 chars) is < MAX_ARG_STRLEN; 8 args/exec < ARG_MAX
)

// appendChunksScript decodes each base64 argv element ($1..$n) and appends the
// bytes to the file named by $0. Paths ride argv so the shell never parses them.
const appendChunksScript = `for a in "$@"; do printf %s "$a" | base64 -d; done >> "$0"`

// readChunkScript emits block $1 (of execChunkRaw bytes) of file $0 as base64.
var readChunkScript = fmt.Sprintf(`dd if="$0" bs=%d skip="$1" count=1 2>/dev/null | base64 -w0`, execChunkRaw)

// copyFileToSandbox writes localPath into the sandbox at remotePath over the
// daemon's reliable request path. It is a thin wrapper over copyReaderToSandbox
// for an on-disk source (the REST upload handler stages the body to a temp file).
func copyFileToSandbox(ctx context.Context, b sandbox.Backend, name, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return copyReaderToSandbox(ctx, b, name, f, fi.Size(), remotePath)
}

// copyReaderToSandbox writes size bytes from r into the sandbox at remotePath over
// the daemon's reliable request path (see the package note above). It streams to a
// temp beside the destination, verifies the byte count, retries the whole stream
// on a short result (re-truncating, so retries never double-append), then
// atomically renames into place. A half-written file is never published. r must be
// seekable so a failed attempt can rewind and retry (an *os.File or *bytes.Reader).
func copyReaderToSandbox(ctx context.Context, b sandbox.Backend, name string, r io.ReadSeeker, size int64, remotePath string) error {
	dir := path.Dir(remotePath)
	// The console's "destination folder" upload invites paths whose folder does
	// not exist yet; create it so the write does not fail.
	if _, err := execChecked(ctx, b, name, "mkdir", "-p", dir); err != nil {
		return fmt.Errorf("create destination dir %s: %w", dir, err)
	}
	tmp := path.Join(dir, ".sbxup-"+path.Base(remotePath)+".part")
	defer func() { _, _ = b.Exec(ctx, name, []string{"rm", "-f", tmp}, sandbox.ExecOpts{}) }()

	var lastErr error
	for try := 0; try < transferTries; try++ {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := streamToSandbox(ctx, b, name, r, tmp); err != nil {
			lastErr = err
			continue
		}
		got, err := remoteSize(ctx, b, name, tmp)
		if err != nil {
			lastErr = err
			continue
		}
		if got == size {
			if _, err := execChecked(ctx, b, name, "mv", "-f", tmp, remotePath); err != nil {
				return err
			}
			return nil
		}
		lastErr = fmt.Errorf("verification failed: wrote %d of %d bytes", got, size)
	}
	return fmt.Errorf("transfer to %s failed after %d attempts: %w", remotePath, transferTries, lastErr)
}

// streamToSandbox truncates tmp, then appends f's contents as base64 exec-argv
// chunks (execChunkBatch per exec). A failed append aborts the attempt; the
// caller re-truncates and retries.
func streamToSandbox(ctx context.Context, b sandbox.Backend, name string, r io.Reader, tmp string) error {
	if _, err := execChecked(ctx, b, name, "sh", "-c", `: > "$0"`, tmp); err != nil {
		return err
	}
	buf := make([]byte, execChunkRaw)
	batch := make([]string, 0, execChunkBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		args := append([]string{"sh", "-c", appendChunksScript, tmp}, batch...)
		_, err := execChecked(ctx, b, name, args...)
		batch = batch[:0]
		return err
	}
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			batch = append(batch, base64.StdEncoding.EncodeToString(buf[:n]))
			if len(batch) == execChunkBatch {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return flush()
}

// copyFileFromSandbox reads remotePath out of the sandbox into localPath over the
// reliable request path: it reads the source in execChunkRaw-byte blocks via
// exec stdout (base64), verifying each block's length and retrying a short read.
func copyFileFromSandbox(ctx context.Context, b sandbox.Backend, name, remotePath, localPath string) error {
	want, err := remoteSize(ctx, b, name, remotePath)
	if err != nil {
		return err
	}
	out, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer out.Close()

	var written int64
	for block := 0; written < want; block++ {
		exp := int64(execChunkRaw)
		if rem := want - written; rem < exp {
			exp = rem
		}
		raw, err := readChunkVerified(ctx, b, name, remotePath, block, int(exp))
		if err != nil {
			return fmt.Errorf("transfer from %s failed: %w", remotePath, err)
		}
		if _, err := out.Write(raw); err != nil {
			return err
		}
		written += int64(len(raw))
	}
	if written != want {
		return fmt.Errorf("download verification failed: got %d of %d bytes", written, want)
	}
	return nil
}

// readChunkVerified returns block (execChunkRaw bytes) of the sandbox file,
// retrying until the decoded length matches want (a short read means the
// exec-stdout transfer truncated).
func readChunkVerified(ctx context.Context, b sandbox.Backend, name, remotePath string, block, want int) ([]byte, error) {
	var lastErr error
	for try := 0; try < transferTries; try++ {
		res, err := execChecked(ctx, b, name, "sh", "-c", readChunkScript, remotePath, strconv.Itoa(block))
		if err != nil {
			lastErr = err
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(res.Stdout)))
		if err != nil {
			lastErr = err
			continue
		}
		if len(raw) == want {
			return raw, nil
		}
		lastErr = fmt.Errorf("short read: %d of %d bytes", len(raw), want)
	}
	return nil, lastErr
}

// remoteSize returns the byte size of a file inside the sandbox via `stat`.
func remoteSize(ctx context.Context, b sandbox.Backend, name, p string) (int64, error) {
	res, err := execChecked(ctx, b, name, "stat", "-c", "%s", p)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(res.Stdout)), 10, 64)
}

// execChecked runs a command in the sandbox and treats a non-zero exit as an error.
func execChecked(ctx context.Context, b sandbox.Backend, name string, args ...string) (sandbox.ExecResult, error) {
	res, err := b.Exec(ctx, name, args, sandbox.ExecOpts{})
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("%s: %s", args[0], strings.TrimSpace(string(res.Stderr)))
	}
	return res, nil
}
