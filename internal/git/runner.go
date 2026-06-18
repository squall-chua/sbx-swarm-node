package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// StepResult is one executed step's outcome.
type StepResult struct {
	Argv     []string
	ExitCode int
	Output   []byte
}

// Runner executes argv steps via os/exec (no shell), restricted to an allowlist
// of binaries (defense in depth on top of config-only commands, ADR-0003).
type Runner struct{ allow map[string]bool }

// NewRunner permits only the given binaries (e.g. "git", "git-lfs").
func NewRunner(allow []string) *Runner {
	m := make(map[string]bool, len(allow))
	for _, a := range allow {
		m[a] = true
	}
	return &Runner{allow: m}
}

// Run executes steps in dir with extra env, stopping at the first failure.
func (r *Runner) Run(ctx context.Context, dir string, env []string, steps [][]string) ([]StepResult, error) {
	var results []StepResult
	for _, argv := range steps {
		if len(argv) == 0 {
			continue
		}
		if !r.allow[argv[0]] {
			return results, fmt.Errorf("binary %q not allowed", argv[0])
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // argv, never a shell string
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), env...)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err := cmd.Run()
		res := StepResult{Argv: argv, Output: buf.Bytes()}
		if err != nil {
			res.ExitCode = -1
			if ee, ok := err.(*exec.ExitError); ok {
				res.ExitCode = ee.ExitCode()
			}
			results = append(results, res)
			return results, fmt.Errorf("step %v failed (exit %d): %s", argv, res.ExitCode, buf.String())
		}
		results = append(results, res)
	}
	return results, nil
}
