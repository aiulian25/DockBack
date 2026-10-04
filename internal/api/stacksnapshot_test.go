package api

import (
	"net/url"
	"testing"
)

// During a real recovery, eleven stack restores ran with "Safety snapshot
// first" ticked and took no snapshot: the box only counted together with
// "Revert update". When one of them destroyed a surviving database, there was
// nothing to go back to.
func TestStackRestoreHonoursTheSnapshotBoxWithoutRecreate(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		{"snapshot=true", true},
		{"snapshot=1", true},
		{"snapshot=true&recreate=false", true}, // the case that silently took nothing
		{"snapshot=true&recreate=true", true},
		{"", true}, // a script that says nothing gets the safe default
		{"snapshot=false", false},
		{"snapshot=0&recreate=true", false},
	}
	for _, tc := range cases {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		if got := stackRestoreSnapshotWanted(q); got != tc.want {
			t.Errorf("%q: snapshot = %v, want %v", tc.query, got, tc.want)
		}
	}
}
