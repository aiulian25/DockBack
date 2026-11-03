package api

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dockback/internal/store"
)

const (
	testTOTPPeriodSeconds = 30
	testTOTPModulus       = 1_000_000
	loginTestIP           = "203.0.113.9"
)

// liveTOTPCode is the authenticator's current code for this account.
func liveTOTPCode(t *testing.T, s *Server, u *store.User) string {
	t.Helper()
	secret, err := s.openSecret(u.TOTPSecret)
	if err != nil {
		t.Fatal(err)
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	counter := make([]byte, 8)
	binary.BigEndian.PutUint64(counter, uint64(time.Now().Unix()/testTOTPPeriodSeconds))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter)
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff%testTOTPModulus)
}

// raceVerify runs verifyUserTOTP with the same code from every racer, all holding the
// same snapshot, which is what requests that read the user at once have.
func raceVerify(s *Server, u *store.User, code string, racers int) int32 {
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.verifyUserTOTP(u, code) {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	return wins.Load()
}

func recoveryLeft(t *testing.T, s *Server) int {
	t.Helper()
	u, err := s.store.GetUserByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	if err := json.Unmarshal([]byte(u.TOTPRecovery), &hashes); err != nil {
		t.Fatal(err)
	}
	return len(hashes)
}

func TestATOTPCodeWorksOnceAcrossARace(t *testing.T) {
	s, u, _ := twofaServer(t)
	if wins := raceVerify(s, u, liveTOTPCode(t, s, u), 16); wins != 1 {
		t.Fatalf("one code, %d successes", wins)
	}
	if s.verifyUserTOTP(u, liveTOTPCode(t, s, u)) {
		t.Fatal("a stale snapshot must not let the code be replayed afterwards")
	}
}

func TestARecoveryCodeWorksOnceAcrossARace(t *testing.T) {
	s, u, display := twofaServer(t)
	if wins := raceVerify(s, u, display[0], 16); wins != 1 {
		t.Fatalf("one recovery code, %d successes", wins)
	}
	if left := recoveryLeft(t, s); left != len(display)-1 {
		t.Fatalf("%d codes left", left)
	}
}

// The lost update: requests holding one snapshot each spend a different code, and none
// may write back the codes the others spent.
func TestDifferentRecoveryCodesAllStaySpent(t *testing.T) {
	s, u, display := twofaServer(t)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, code := range display {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.verifyUserTOTP(u, code) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if int(wins.Load()) != len(display) {
		t.Fatalf("%d of %d distinct codes worked", wins.Load(), len(display))
	}
	if left := recoveryLeft(t, s); left != 0 {
		t.Fatalf("%d spent codes came back", left)
	}
	for _, code := range display {
		if s.verifyUserTOTP(u, code) {
			t.Fatalf("spent code %q worked again", code)
		}
	}
}

// A code whose use cannot be saved could be used again, so it grants nothing.
func TestAFactorThatCannotBeSavedGrantsNothing(t *testing.T) {
	s, u, display := twofaServer(t)
	code := liveTOTPCode(t, s, u)
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	if s.verifyUserTOTP(u, code) {
		t.Error("a TOTP code was accepted with its use unsaved")
	}
	if s.verifyUserTOTP(u, display[0]) {
		t.Error("a recovery code was accepted with its use unsaved")
	}
}

func TestAWrongCodeSpendsNothing(t *testing.T) {
	s, u, display := twofaServer(t)
	if s.verifyUserTOTP(u, "not-a-code") {
		t.Fatal("a wrong code worked")
	}
	if left := recoveryLeft(t, s); left != len(display) {
		t.Fatalf("%d codes left", left)
	}
	if !s.verifyUserTOTP(u, liveTOTPCode(t, s, u)) {
		t.Fatal("the live code must still work")
	}
	if !s.verifyUserTOTP(u, display[0]) {
		t.Fatal("an unspent recovery code must still work")
	}
}

func TestLoginGrantsOneSessionPerCode(t *testing.T) {
	cases := map[string]func(*testing.T, *Server, *store.User, []string) string{
		"totp":     func(t *testing.T, s *Server, u *store.User, _ []string) string { return liveTOTPCode(t, s, u) },
		"recovery": func(_ *testing.T, _ *Server, _ *store.User, display []string) string { return display[0] },
	}
	for name, codeFor := range cases {
		t.Run(name, func(t *testing.T) {
			s, u, display := twofaServer(t)
			code := codeFor(t, s, u, display)
			const racers = 3
			var ok atomic.Int32
			var wg sync.WaitGroup
			for range racers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if postLogin(s, loginTestIP, "admin", rsPassword, code).Code == http.StatusOK {
						ok.Add(1)
					}
				}()
			}
			wg.Wait()
			if ok.Load() != 1 {
				t.Fatalf("%d logins from one code", ok.Load())
			}
			if rec := postLogin(s, loginTestIP, "admin", rsPassword, code); rec.Code == http.StatusOK {
				t.Fatal("the code logged in again afterwards")
			}
		})
	}
}
