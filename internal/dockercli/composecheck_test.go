package dockercli

import (
	"fmt"
	"testing"
)

func TestParseConfigHashes(t *testing.T) {
	got := parseConfigHashes("app 21af4c74\ndb 4235f730\n\n")
	if fmt.Sprint(got) != "map[app:21af4c74 db:4235f730]" {
		t.Errorf("got %v", got)
	}
}

// Compose prints logrus-style lines; the restore log wants the sentence.
func TestComposeMessages(t *testing.T) {
	out := `time="2026-10-04T21:13:03Z" level=warning msg="The \"UNSET_THING\" variable is not set. Defaulting to a blank string."
env file /srv/app/app.env not found: stat /srv/app/app.env: no such file or directory
`
	got := composeMessages(out)
	if len(got) != 2 || got[0] != `The "UNSET_THING" variable is not set. Defaulting to a blank string.` || got[1] != "env file /srv/app/app.env not found: stat /srv/app/app.env: no such file or directory" {
		t.Errorf("got %q", got)
	}
}
