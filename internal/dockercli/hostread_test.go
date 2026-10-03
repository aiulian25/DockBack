package dockercli

import (
	"strings"
	"testing"
)

// The sidecar reader must accept and refuse exactly what the SSH reader does.
// Two validators would mean the paths a backup is willing to read depend on the
// transport a node happens to use, and the looser one would be the one nobody
// noticed — so this asserts they are literally the same function.
func TestReadHostFileSharesTheSSHPathGrammar(t *testing.T) {
	for _, p := range []string{
		"/volume1/docker/webapp/docker-compose.yml",
		"/home/user/docker/gc/.env",
		"/opt/stacks/my project/compose.yaml", // spaces are legal in the grammar
	} {
		if err := ValidateFetchPath(p); err != nil {
			t.Errorf("%q is an ordinary compose path and must be readable: %v", p, err)
		}
	}
	for _, p := range []string{
		"",                         // nothing
		"relative/compose.yml",     // not absolute
		"/srv/../../etc/shadow",    // traversal
		"/srv/app/$(id).yml",       // command substitution
		"/srv/app/a;rm -rf /.yml",  // separator
		"/srv/app/a`id`.yml",       // backtick
		"/srv/app/a|b.yml",         // pipe
		"/srv/app/a\nb.yml",        // newline
		strings.Repeat("/a", 3000), // over the length bound
	} {
		if err := ValidateFetchPath(p); err == nil {
			t.Errorf("%q must be refused by the shared path grammar", p)
		}
	}
}

// The cap is applied INSIDE the container, so an enormous file is never carried
// into this process to be measured and thrown away. One byte over is requested
// deliberately: it is what separates "exactly at the limit" from "truncated".
func TestHostReadScriptBoundsTheReadAtTheSource(t *testing.T) {
	script := hostReadScript("/volume1/docker/gc/.env", 1<<20)
	if !strings.Contains(script, "head -c 1048577 ") {
		t.Errorf("the read must be bounded at maxBytes+1 by head, got: %s", script)
	}
	if !strings.Contains(script, "'"+hostReadMount+"/volume1/docker/gc/.env'") {
		t.Errorf("the path must be read under the read-only host mount and quoted, got: %s", script)
	}
	// The grammar already excludes quotes, but the quoting is what makes a legal
	// space in a path reach `head` as one argument.
	spaced := hostReadScript("/opt/my stacks/.env", 4096)
	if !strings.Contains(spaced, "'"+hostReadMount+"/opt/my stacks/.env'") {
		t.Errorf("a path containing a space must be passed as one argument, got: %s", spaced)
	}
}

// Nothing about a read may create anything. Binding the file's PARENT would:
// Docker creates a missing bind source before mounting it even for :ro, so a
// stale compose path would leave an empty directory behind on the host.
func TestHostReadMountIsReadOnlyRoot(t *testing.T) {
	if hostReadMount == "" || !strings.HasPrefix(hostReadMount, "/") {
		t.Fatalf("the host read mount must be an absolute path, got %q", hostReadMount)
	}
	// Mirrors the bind this builds: "/:<mount>:ro". Asserted as a string so a
	// change from :ro to :rw cannot pass unnoticed.
	if got := "/:" + hostReadMount + ":ro"; !strings.HasSuffix(got, ":ro") {
		t.Errorf("the host filesystem must be bound read-only, got %q", got)
	}
}

// The permission audit now reads the owner alongside the mode, because for a
// private key the two answer one question together. Three-field lines still
// parse: an audit taken before the owner was read must not stop being readable.
func TestParseFileModesReadsTheOwner(t *testing.T) {
	got := ParseFileModes(strings.Join([]string{
		"MODE\t/run/secrets/vapid_private_key\t600\t1026:100",
		"MODE\t/legacy/key\t600", // three fields, as older output had
		"MODE\t/missing/key\t-\t-",
		"MODE\t/odd/key\t600\troot:root", // non-numeric owner is dropped, mode kept
		"noise",
		"MODE\t/too/many\t600\t1:1\textra",
	}, "\n"))

	if len(got) != 4 {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	if got[0].Owner != "1026:100" || got[0].Mode != "600" {
		t.Errorf("owner and mode must both be read: %+v", got[0])
	}
	if got[1].Owner != "" || got[1].Mode != "600" {
		t.Errorf("a three-field line keeps its mode and claims no owner: %+v", got[1])
	}
	if !got[2].Missing {
		t.Errorf("an unreadable path is missing, not owned: %+v", got[2])
	}
	if got[3].Owner != "" {
		t.Errorf("a non-numeric owner must be dropped — it would reach a chown suggestion: %+v", got[3])
	}
}
