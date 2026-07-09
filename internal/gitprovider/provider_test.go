package gitprovider

import "testing"

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
