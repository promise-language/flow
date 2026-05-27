package github

import "testing"

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		raw   string
		owner string
		repo  string
		bad   bool
	}{
		{"https://github.com/owner/repo.git", "owner", "repo", false},
		{"https://github.com/owner/repo", "owner", "repo", false},
		{"git@github.com:owner/repo.git", "owner", "repo", false},
		{"ssh://git@github.com/owner/repo.git", "owner", "repo", false},
		{"https://gitlab.com/owner/repo.git", "owner", "repo", false}, // permissive
		{"not-a-url", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			owner, repo, err := parseGitHubRemote(c.raw)
			if c.bad {
				if err == nil {
					t.Errorf("expected error for %q", c.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err for %q: %v", c.raw, err)
			}
			if owner != c.owner || repo != c.repo {
				t.Errorf("got (%q,%q), want (%q,%q)", owner, repo, c.owner, c.repo)
			}
		})
	}
}
