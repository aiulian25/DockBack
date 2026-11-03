package store

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

const consumeSecret = "sealed-secret"

func factorUser(t *testing.T, recovery []string, lastStep int64) (*Store, int64) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id, err := st.CreateUser("admin", "hash")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(recovery)
	if err := st.EnableTOTP(id, consumeSecret, string(encoded), lastStep); err != nil {
		t.Fatal(err)
	}
	return st, id
}

func storedRecovery(t *testing.T, st *Store) []string {
	t.Helper()
	u, err := st.GetUserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	if err := json.Unmarshal([]byte(u.TOTPRecovery), &codes); err != nil {
		t.Fatal(err)
	}
	return codes
}

// removeCode is the shape the caller's match takes.
func removeCode(code string) func(string) (string, bool) {
	return func(current string) (string, bool) {
		var codes []string
		_ = json.Unmarshal([]byte(current), &codes)
		at := slices.Index(codes, code)
		if at < 0 {
			return "", false
		}
		encoded, _ := json.Marshal(slices.Delete(codes, at, at+1))
		return string(encoded), true
	}
}

func TestConsumeTOTPStepHasOneWinner(t *testing.T) {
	st, id := factorUser(t, nil, 100)
	const racers = 16
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := st.ConsumeTOTPStep(id, consumeSecret, 101)
			if err != nil {
				t.Error(err)
			}
			if won {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("one step, %d winners", wins.Load())
	}
}

func TestConsumeTOTPStepRefuses(t *testing.T) {
	st, id := factorUser(t, nil, 100)
	cases := []struct {
		name   string
		sealed string
		step   int64
	}{
		{"an already spent step", consumeSecret, 100},
		{"an older step", consumeSecret, 99},
		{"a secret that has since changed", "another-secret", 101},
	}
	for _, c := range cases {
		if won, err := st.ConsumeTOTPStep(id, c.sealed, c.step); err != nil || won {
			t.Errorf("%s: won %v, err %v", c.name, won, err)
		}
	}
	if won, err := st.ConsumeTOTPStep(id, consumeSecret, 101); err != nil || !won {
		t.Fatalf("a newer step on the current secret must win: %v, %v", won, err)
	}
}

func TestOneRecoveryCodeHasOneWinner(t *testing.T) {
	st, id := factorUser(t, []string{"a", "b"}, 0)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, won, err := st.ConsumeRecoveryCode(id, removeCode("a"))
			if err != nil {
				t.Error(err)
			}
			if won {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("one code, %d winners", wins.Load())
	}
	if got := storedRecovery(t, st); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("remaining = %v", got)
	}
}

// The lost update the finding describes: a write from a stale list would bring back a
// code someone else spent. match runs against a list another request then changes.
func TestALostSwapReadsAgainInsteadOfRestoringACode(t *testing.T) {
	st, id := factorUser(t, []string{"a", "b"}, 0)
	interfered := false
	remaining, won, err := st.ConsumeRecoveryCode(id, func(current string) (string, bool) {
		if !interfered {
			interfered = true
			if _, otherWon, oerr := st.ConsumeRecoveryCode(id, removeCode("b")); oerr != nil || !otherWon {
				t.Fatalf("the other request: %v, %v", otherWon, oerr)
			}
		}
		return removeCode("a")(current)
	})
	if err != nil || !won || remaining != "[]" {
		t.Fatalf("won %v, remaining %q, err %v", won, remaining, err)
	}
	if got := storedRecovery(t, st); len(got) != 0 {
		t.Fatalf("a spent code came back: %v", got)
	}
}

func TestRecoveryConsumeGivesUpRatherThanSpinning(t *testing.T) {
	st, id := factorUser(t, []string{"a"}, 0)
	version := 0
	_, won, err := st.ConsumeRecoveryCode(id, func(string) (string, bool) {
		version++
		encoded, _ := json.Marshal([]string{"changed", string(rune('a' + version))})
		if err := st.EnableTOTP(id, consumeSecret, string(encoded), 0); err != nil {
			t.Fatal(err)
		}
		return "[]", true
	})
	if won || err == nil {
		t.Fatalf("won %v, err %v", won, err)
	}
	if version != recoveryConsumeAttempts {
		t.Fatalf("%d attempts", version)
	}
}

func TestConsumeReportsAStorageFailure(t *testing.T) {
	st, id := factorUser(t, []string{"a"}, 0)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if won, err := st.ConsumeTOTPStep(id, consumeSecret, 1); won || err == nil {
		t.Errorf("step: won %v, err %v", won, err)
	}
	_, won, err := st.ConsumeRecoveryCode(id, removeCode("a"))
	if won || err == nil {
		t.Errorf("recovery: won %v, err %v", won, err)
	}
}
