package api

import (
	"net/url"
	"testing"

	"dockback/internal/backup"
)

// The capacity verdict is the one number an operator acts on before committing
// a stack restore. Sized over the whole stack while half of it is deselected,
// it can refuse a restore that would comfortably fit — a refusal with no
// remedy, since deselecting more does not change it.
func planEntries(names ...string) []backup.StackPlanEntry {
	out := make([]backup.StackPlanEntry, 0, len(names))
	for _, n := range names {
		out = append(out, backup.StackPlanEntry{Service: n})
	}
	return out
}

func serviceNames(entries []backup.StackPlanEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Service)
	}
	return out
}

func TestSelectedEntriesNarrowsToTheKeptServices(t *testing.T) {
	all := planEntries("db", "api", "web")

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"a subset", "db,web", []string{"db", "web"}},
		{"one service", "api", []string{"api"}},
		{"spaces are tolerated", " db , api ", []string{"db", "api"}},
		// A client that sends nothing means the whole stack, so the endpoint
		// behaves exactly as it did before the parameter existed.
		{"absent", "", []string{"db", "api", "web"}},
		{"empty list", ",,", []string{"db", "api", "web"}},
		// This panel refreshes mid-selection; an unknown name is stale input, not
		// an error, and must not silently size the restore at zero.
		{"only unknown names", "ghost", []string{"db", "api", "web"}},
		{"unknown names are ignored", "db,ghost", []string{"db"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serviceNames(selectedEntries(url.Values{"services": {tc.query}}, all))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v (order must follow the plan)", got, tc.want)
				}
			}
		})
	}

	// The caller still gets the full list for the picker.
	if len(all) != 3 {
		t.Errorf("the plan itself must not be mutated: %v", serviceNames(all))
	}
}
