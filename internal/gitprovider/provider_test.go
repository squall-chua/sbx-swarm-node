package gitprovider

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDerive(t *testing.T) {
	cases := []struct {
		url, explicit string
		want          Provider
	}{
		{"https://github.com/acme/app", "", GitHub},
		{"git@github.com:acme/app.git", "", GitHub},
		{"https://gitlab.com/acme/app", "", GitLab},
		{"https://gitlab.corp.internal/acme/app", "", GitLab},
		{"https://github.corp.internal/acme/app", "", Plain},  // enterprise not host-derivable
		{"ssh://git@gerrit.corp:29418/svc", "", Plain},        // gerrit never derived
		{"ssh://git@gerrit.corp:29418/svc", "gerrit", Gerrit}, // explicit wins
		{"https://github.com/acme/app", "gitlab", GitLab},     // explicit overrides host
	}
	for _, c := range cases {
		if got := Derive(c.url, c.explicit); got != c.want {
			t.Errorf("Derive(%q,%q)=%q want %q", c.url, c.explicit, got, c.want)
		}
	}
}

func TestAPIBase(t *testing.T) {
	cases := []struct {
		p             Provider
		url, override string
		want          string
	}{
		{GitHub, "https://github.com/acme/app", "", "https://api.github.com"},
		{GitHub, "https://ghe.corp.com/acme/app", "", "https://ghe.corp.com/api/v3"},
		{GitLab, "https://gitlab.com/acme/app", "", "https://gitlab.com/api/v4"},
		{GitLab, "https://gitlab.corp/g/s/app", "", "https://gitlab.corp/api/v4"},
		{Gerrit, "ssh://git@gerrit.corp:29418/svc", "", ""},
		{Plain, "https://x/y/z", "", ""},
		{GitHub, "https://github.com/acme/app", "https://override/api", "https://override/api"},
	}
	for _, c := range cases {
		if got := APIBase(c.p, c.url, c.override); got != c.want {
			t.Errorf("APIBase(%q,%q,%q)=%q want %q", c.p, c.url, c.override, got, c.want)
		}
	}
}

func TestParseRepo(t *testing.T) {
	// GitHub: exactly owner/repo, both URL forms, .git stripped.
	o, r, err := ParseRepo(GitHub, "https://github.com/acme/app.git")
	require.NoError(t, err)
	require.Equal(t, "acme", o)
	require.Equal(t, "app", r)
	o, r, err = ParseRepo(GitHub, "git@github.com:acme/app.git")
	require.NoError(t, err)
	require.Equal(t, "acme", o)
	require.Equal(t, "app", r)
	// GitHub with a nested path is not a valid repo.
	_, _, err = ParseRepo(GitHub, "https://github.com/acme")
	require.Error(t, err)
	_, _, err = ParseRepo(GitHub, "https://github.com/a/b/c")
	require.Error(t, err)
	// GitLab: whole path is the project (subgroups kept), owner empty.
	o, r, err = ParseRepo(GitLab, "https://gitlab.corp/group/sub/app.git")
	require.NoError(t, err)
	require.Equal(t, "", o)
	require.Equal(t, "group/sub/app", r)
	// Empty path rejected.
	_, _, err = ParseRepo(GitLab, "https://gitlab.corp/")
	require.Error(t, err)
}

func TestSupports(t *testing.T) {
	if !Plain.Supports("branch") || !Plain.Supports("patch") {
		t.Fatal("plain must support branch+patch")
	}
	if Plain.Supports("pull_request") || Plain.Supports("gerrit_change") {
		t.Fatal("plain must reject pull_request/gerrit_change")
	}
	if !GitHub.Supports("pull_request") || GitHub.Supports("gerrit_change") {
		t.Fatal("github supports PR, not gerrit")
	}
	if !Gerrit.Supports("gerrit_change") || Gerrit.Supports("pull_request") {
		t.Fatal("gerrit supports gerrit_change, not PR")
	}
}
