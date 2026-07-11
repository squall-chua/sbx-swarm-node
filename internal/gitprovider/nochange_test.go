package gitprovider

import "testing"

func TestPushUpToDate(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"up to date", "To github.com:o/r.git\n=\trefs/heads/x:refs/heads/x\t[up to date]\nDone\n", true},
		{"fast-forward", "To github.com:o/r.git\n \trefs/heads/x:refs/heads/x\tabc..def\nDone\n", false},
		{"new branch", "To github.com:o/r.git\n*\trefs/heads/x:refs/heads/x\t[new branch]\nDone\n", false},
		{"empty output", "", false},
	}
	for _, c := range cases {
		if got := pushUpToDate([]byte(c.out)); got != c.want {
			t.Errorf("%s: pushUpToDate=%v want %v", c.name, got, c.want)
		}
	}
}
