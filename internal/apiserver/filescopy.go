package apiserver

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// uploadCopyTries bounds the retry loop in copyFileToSandbox. The sbx daemon's
// file transport intermittently truncates a transfer ("tar: Unexpected EOF");
// the failure is not size-correlated and clears on a retry, so a small bound is
// enough (live runs converged in 1–2 attempts).
const uploadCopyTries = 8

// copyFileToSandbox copies localPath into the sandbox at remotePath, defending
// against the daemon's intermittently-truncating transfer (see uploadCopyTries).
// It copies to a fresh temp path beside the destination, verifies the landed
// byte count, retries on a short transfer, then atomically renames the verified
// file into place. A copy that never verifies is an error and the partial is
// removed — a half-written file is never published to the destination.
func copyFileToSandbox(ctx context.Context, b sandbox.Backend, name, localPath, remotePath string) error {
	fi, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	want := fi.Size()
	// Temps live in the destination directory so the final mv is a same-filesystem
	// rename (atomic). Each attempt uses a FRESH path: a truncated transfer leaves
	// a root-owned partial that the daemon cannot overwrite, so reusing one path
	// would poison every retry. We clean up all attempt temps at the end.
	dir := path.Dir(remotePath)
	base := ".sbxup-" + filepath.Base(localPath)
	var temps []string
	defer func() {
		for _, tp := range temps {
			_, _ = b.Exec(ctx, name, []string{"rm", "-f", tp}, sandbox.ExecOpts{})
		}
	}()

	var lastErr error
	for try := 0; try < uploadCopyTries; try++ {
		tmp := path.Join(dir, fmt.Sprintf("%s.%d.part", base, try))
		temps = append(temps, tmp)
		if err := b.CopyTo(ctx, name, localPath, tmp); err != nil {
			lastErr = err
			continue
		}
		got, err := remoteSize(ctx, b, name, tmp)
		if err != nil {
			lastErr = err
			continue
		}
		if got == want {
			if _, err := execChecked(ctx, b, name, "mv", "-f", tmp, remotePath); err != nil {
				return err
			}
			return nil
		}
		lastErr = fmt.Errorf("transfer truncated: %d of %d bytes", got, want)
	}
	return fmt.Errorf("transfer to %s failed after %d attempts: %w", remotePath, uploadCopyTries, lastErr)
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
