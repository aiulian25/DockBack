package api

import (
	"errors"
	"testing"

	"dockback/internal/dockercli"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// F67: a refused SSH pin mismatch raises ONE critical alert per (node, new
// key) — deduped across ticks — and is detectable via errors.Is/As.

func TestHostKeyChangedErrorIdentity(t *testing.T) {
	err := &dockercli.HostKeyChangedError{Host: "203.0.113.7:22", OldFP: "SHA256:aaaa", NewFP: "SHA256:bbbb"}
	if !errors.Is(err, dockercli.ErrHostKeyChanged) {
		t.Fatal("typed error must unwrap to the sentinel")
	}
	var hk *dockercli.HostKeyChangedError
	if !errors.As(error(err), &hk) || hk.NewFP != "SHA256:bbbb" {
		t.Fatal("errors.As must recover the fingerprints")
	}
	// A generic connect error never matches.
	if errors.Is(errors.New("dial tcp: connection refused"), dockercli.ErrHostKeyChanged) {
		t.Fatal("generic errors must not match the sentinel")
	}
}

func TestHostKeyAlertDeduped(t *testing.T) {
	st := testStore(t)
	s := &Server{store: st, notifier: notify.New(func() (notify.Config, error) { return notify.Config{}, nil }, func(string, string) {})}
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "curio"}); err != nil {
		t.Fatal(err)
	}
	hk := &dockercli.HostKeyChangedError{Host: "host:22", OldFP: "SHA256:old1", NewFP: "SHA256:new1"}

	countAlerts := func() int {
		alerts, _ := st.ListAlerts(false, 100, 0)
		n := 0
		for _, a := range alerts {
			if a.Kind == notify.KindHostKeyChanged {
				n++
			}
		}
		return n
	}

	// First tick: exactly one critical alert row.
	s.alertHostKeyChanged("n1", hk)
	if got := countAlerts(); got != 1 {
		t.Fatalf("first change: %d alerts, want 1", got)
	}
	alerts, _ := st.ListAlerts(false, 100, 0)
	for _, a := range alerts {
		if a.Kind == notify.KindHostKeyChanged && a.Severity != "critical" {
			t.Fatalf("severity = %q, want critical", a.Severity)
		}
	}

	// Second tick, same key: deduped — still one row.
	s.alertHostKeyChanged("n1", hk)
	if got := countAlerts(); got != 1 {
		t.Fatalf("same-key repeat must dedup, got %d", got)
	}

	// The key changes AGAIN (a different presented key): a fresh alert fires
	// immediately — new dedup scope.
	hk2 := &dockercli.HostKeyChangedError{Host: "host:22", OldFP: "SHA256:old1", NewFP: "SHA256:new2"}
	s.alertHostKeyChanged("n1", hk2)
	if got := countAlerts(); got != 2 {
		t.Fatalf("a NEW key must alert again, got %d", got)
	}
}
