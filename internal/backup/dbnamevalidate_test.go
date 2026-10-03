package backup

import (
	"strings"
	"testing"
)

// The names in a backup request travel from an HTTP body into a client run as
// root inside the database container.

func TestValidDBNameAcceptsRealNames(t *testing.T) {
	// Every one of these is an ordinary database name. Rejecting any of them
	// would break a real selection to guard against nothing — which is why this
	// is not cleanSQLIdent's letters-and-digits alphabet.
	for _, name := range []string{
		"appdb", "paperless", "nextcloud_db", "my-app", "my-app-prod",
		"bookstack.v2", "DB1", "a", strings.Repeat("n", maxDBNameLen),
	} {
		if !ValidDBName(name) {
			t.Errorf("%q is an ordinary database name and must be accepted", name)
		}
	}
}

func TestValidDBNameRejectsAnythingAShellCouldRead(t *testing.T) {
	cases := map[string]string{
		"quote breakout":      "appdb'; touch /tmp/x; echo '",
		"backtick":            "a`id`b",
		"dollar substitution": "a$(id)b",
		"semicolon":           "appdb; rm -rf /",
		"pipe":                "appdb | sh",
		"newline":             "appdb\nrm -rf /",
		"space":               "app db",
		"glob":                "*",
		"path":                "../../etc/passwd",
		"leading dash":        "--all-databases",
		"single dash":         "-x",
		"leading dot":         ".hidden",
		"empty":               "",
		"too long":            strings.Repeat("n", maxDBNameLen+1),
	}
	for what, name := range cases {
		if ValidDBName(name) {
			t.Errorf("%s: %q must be refused before it reaches a shell", what, name)
		}
	}
}

func TestInvalidDBNamesReportsThemAllAtOnce(t *testing.T) {
	got := InvalidDBNames([]string{"appdb", "*", "  ", "my-app", "a;b"})
	if len(got) != 2 || got[0] != "*" || got[1] != "a;b" {
		t.Fatalf("got %v, want every bad name and only the bad ones", got)
	}
	// Blank entries are dropped elsewhere and are not a caller error.
	if len(InvalidDBNames([]string{"", "   ", "appdb"})) != 0 {
		t.Error("a blank entry is not an invalid name")
	}
	if len(InvalidDBNames(nil)) != 0 {
		t.Error("no selection means nothing to reject")
	}
}

// The quoting these names rely on is what makes them inert today. This pins it,
// because the validator above is the belt and this is the braces.
func TestSelectedNamesAreQuotedIntoOneArgument(t *testing.T) {
	hostile := "appdb'; touch /tmp/pwned; echo '"
	env := []string{"POSTGRES_USER=app", "MYSQL_ROOT_PASSWORD=r"}

	for name, cmd := range map[string][]string{
		"postgres": dbDumpCommand("postgres", env, []string{hostile}),
		"mysql":    dbDumpCommand("mysql", env, []string{hostile}),
	} {
		script := cmd[2]
		// The dangerous reading is the name ESCAPING its quotes. shellEscape turns
		// each ' into '\'' so the value stays one word.
		if strings.Contains(script, hostile) {
			t.Errorf("%s: the name is unescaped in the script, so it would break out: %s", name, script)
		}
		if !strings.Contains(script, `appdb'\''; touch /tmp/pwned; echo '\''`) {
			t.Errorf("%s: the name must be single-quote escaped: %s", name, script)
		}
	}
}

// The full-cluster MySQL dump splits an in-container list on whitespace, which
// must not also glob, and must not accept a name the client would read as a flag.
func TestFullClusterDumpDoesNotGlobOrTakeOptions(t *testing.T) {
	script := dbDumpCommand("mysql", []string{"MYSQL_ROOT_PASSWORD=r"}, nil)[2]
	if !strings.Contains(script, "set -f") {
		t.Error("pathname expansion must be off: a database named * expanded to the working directory's files")
	}
	if !strings.Contains(script, `case "$d" in -*)`) {
		t.Error("a database whose name begins with a dash would be read as an option — that must be refused loudly")
	}
	if !strings.Contains(script, "exit 5") {
		t.Error("the refusal must be a distinct, loud failure")
	}
}
