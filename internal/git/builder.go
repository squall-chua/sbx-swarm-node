// Package git runs declarative, shell-free git pipelines for clone-mode
// workspaces (ADR-0003). Commands come from node config; only Vars below are
// request-supplied, and they are validated and bound as discrete argv.
package git

import (
	"fmt"
	"regexp"
	"strings"
)

// Vars are the values bound into pipeline steps. Only Branch is request-supplied;
// the rest are config-/runtime-derived. All are validated as git refs.
type Vars struct {
	Branch        string
	BaseRef       string
	Remote        string
	SandboxRemote string
}

// refOK: no leading '-', no control chars/spaces, no '..' (checked separately).
var refOK = regexp.MustCompile(`^[A-Za-z0-9._/\-]+$`)

func validateRef(name, val string) error {
	if val == "" {
		return nil // unset: a step may simply not reference it
	}
	if strings.HasPrefix(val, "-") || strings.Contains(val, "..") || !refOK.MatchString(val) {
		return fmt.Errorf("invalid %s %q", name, val)
	}
	return nil
}

// Build substitutes vars into each step's argv tokens, after validating every
// value as a git ref. Values are bound as discrete argv elements (never
// shell-interpreted).
func Build(steps [][]string, v Vars) ([][]string, error) {
	for _, f := range []struct{ name, val string }{
		{"branch", v.Branch}, {"base_ref", v.BaseRef},
		{"remote", v.Remote}, {"sandbox_remote", v.SandboxRemote},
	} {
		if err := validateRef(f.name, f.val); err != nil {
			return nil, err
		}
	}
	repl := strings.NewReplacer(
		"{branch}", v.Branch, "{base_ref}", v.BaseRef,
		"{remote}", v.Remote, "{sandbox_remote}", v.SandboxRemote,
	)
	out := make([][]string, len(steps))
	for i, step := range steps {
		argv := make([]string, len(step))
		for j, tok := range step {
			argv[j] = repl.Replace(tok)
		}
		out[i] = argv
	}
	return out, nil
}
