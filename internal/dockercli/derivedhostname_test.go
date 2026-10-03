package dockercli

import "testing"

// Docker gives a container the first 12 characters of its own id as a hostname
// when none was chosen. That value describes the container it was generated for
// and nothing else — so replaying it onto a recreated container pins a name
// matching a container that no longer exists, which nothing on the network
// resolves and no sibling can reach it by.
//
// The compose reconstruction already refused to write such a name down. The
// recreate replayed it. One rule now, because the two disagreeing is a stack
// that comes back resolvable by one route and not the other.

const containerID = "9f2c1ab34de5f6789abcdef0123456789abcdef0123456789abcdef012345678"

func TestDerivedHostnameRecognisesDockersOwnDefault(t *testing.T) {
	derived := map[string]string{
		"the short id":       containerID[:12],
		"nothing set at all": "",
	}
	for what, hostname := range derived {
		if !DerivedHostname(hostname, containerID) {
			t.Errorf("%s (%q) is Docker's own, not a choice", what, hostname)
		}
	}
}

func TestDerivedHostnameKeepsAChosenName(t *testing.T) {
	chosen := map[string]string{
		"a service name":            "paperless-db",
		"a fully-qualified name":    "db.internal.example.com",
		"twelve chars, not the id":  "abcdefabcdef",
		"twelve chars, another id":  "0123456789ab",
		"short but real":            "db",
		"hex-looking but too short": "9f2c1ab34de",
		"the id's first 13":         containerID[:13],
	}
	for what, hostname := range chosen {
		if DerivedHostname(hostname, containerID) {
			t.Errorf("%s (%q) was chosen by somebody and must be kept", what, hostname)
		}
	}
}

// The rule must not depend on a full id being available: a manifest records the
// id it captured, and a short or empty one must never make a real hostname look
// derived.
func TestDerivedHostnameWithAnUnhelpfulID(t *testing.T) {
	if DerivedHostname("paperless-db", "") {
		t.Error("no recorded id cannot make a chosen name look derived")
	}
	if DerivedHostname("abcdefabcdef", "abc") {
		t.Error("a short id must not match a full-length hostname")
	}
	// The prefix must be the WHOLE hostname, not merely start with it.
	if !DerivedHostname("9f2c1ab34de5", containerID) {
		t.Error("the short id itself is derived")
	}
}

// The second half of the fix. Aliases are built AFTER the recreate blanks a
// derived hostname, so the stale 12-hex name is not advertised on the network
// either — which would have been the same broken name by another route.
func TestRecreateDropsDerivedHostnameFromAliasesToo(t *testing.T) {
	recorded := []string{containerID[:12], "db", "paperless-db"}

	// What cleanAliases receives once the recreate has blanked a derived name.
	got := cleanAliases(recorded, "")
	for _, a := range got {
		if a == containerID[:12] {
			t.Errorf("the old container's short id must not be advertised: %v", got)
		}
	}
	for _, want := range []string{"db", "paperless-db"} {
		if !hasAlias(got, want) {
			t.Errorf("a real alias was dropped: %v", got)
		}
	}

	// A chosen hostname is still prepended, because that is the name the
	// application's siblings look it up by.
	withChosen := cleanAliases(recorded, "paperless-db")
	if len(withChosen) == 0 || withChosen[0] != "paperless-db" {
		t.Errorf("a chosen hostname must lead the alias list: %v", withChosen)
	}
	// …and only once.
	seen := 0
	for _, a := range withChosen {
		if a == "paperless-db" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the hostname must appear once, got %d: %v", seen, withChosen)
	}
}

func hasAlias(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
