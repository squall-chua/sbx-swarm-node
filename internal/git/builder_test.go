package git

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuild_SubstitutesValidatedValues(t *testing.T) {
	vars := Vars{Branch: "agent/task-1", BaseRef: "main", Remote: "origin", SandboxRemote: "sandbox-n1.abc"}
	steps := [][]string{
		{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"},
		{"git", "push", "{remote}", "{branch}"},
	}
	argv, err := Build(steps, vars)
	require.NoError(t, err)
	require.Equal(t, []string{"git", "fetch", "sandbox-n1.abc", "+refs/heads/agent/task-1:refs/heads/agent/task-1"}, argv[0])
	require.Equal(t, []string{"git", "push", "origin", "agent/task-1"}, argv[1])
}

func TestBuild_RejectsInjection(t *testing.T) {
	_, err := Build([][]string{{"git", "checkout", "{branch}"}}, Vars{Branch: "--upload-pack=evil"})
	require.Error(t, err) // leading '-' rejected
	_, err = Build([][]string{{"git", "checkout", "{branch}"}}, Vars{Branch: "bad\nname"})
	require.Error(t, err) // control char rejected
	_, err = Build([][]string{{"git", "checkout", "{branch}"}}, Vars{Branch: "a..b"})
	require.Error(t, err) // ".." rejected
}

func TestBuild_EmptyValueAllowed(t *testing.T) {
	// An unset value is fine; a step simply may not reference it.
	_, err := Build([][]string{{"git", "fetch", "{remote}"}}, Vars{Remote: "origin"})
	require.NoError(t, err)
}
