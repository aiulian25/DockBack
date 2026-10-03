package dockercli

import "testing"

func TestFetchPathValidator(t *testing.T) {
	cases := []struct {
		path string
		ok   bool
	}{
		// Accepted: real, absolute config-file paths.
		{"/home/user/docker/app/docker-compose.yml", true},
		{"/opt/stacks/my-app/compose.yaml", true},
		{"/srv/dir with space/compose.yml", true}, // space is allowed, single-quoted remotely
		{"/a/b-c_d.e/compose.override.yml", true},

		// Rejected: traversal.
		{"/etc/../etc/shadow", false},
		{"/../secret", false},
		{"/a/../../etc/passwd", false},

		// Rejected: relative.
		{"etc/passwd", false},
		{"./compose.yml", false},
		{"", false},

		// Rejected: shell metacharacters / injection shapes.
		{"/path;rm -rf /", false},
		{"/path`whoami`", false},
		{"/path$(id)", false},
		{"/path|cat", false},
		{"/path&&echo", false},
		{"/path'quote", false},
		{`/path"dq`, false},
		{`/path\esc`, false},
		{"/path\nnewline", false},
		{"/path>redir", false},
	}
	for _, c := range cases {
		err := validateFetchPath(c.path)
		if c.ok && err != nil {
			t.Errorf("expected %q to be ACCEPTED, got error: %v", c.path, err)
		}
		if !c.ok && err == nil {
			t.Errorf("expected %q to be REJECTED, but it passed", c.path)
		}
	}
}
