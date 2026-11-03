package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"dockback/internal/store"
)

// The window a pre-hook opens: what runs, in what order, and what is said about it.
func hookFixture(t *testing.T, pre, post []Hook, failing ...string) (*hookBracket, *[]string, *[]string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	alerts := &[]string{}
	e := &Engine{Store: st, Log: func(string, string, string) {}, Notify: func(_, title, _ string) { *alerts = append(*alerts, title) }}
	bracket := e.newHookBracket(nil, "n1", "cid", "app", "image:1", "op", pre, post)
	ran := &[]string{}
	bracket.exec = func(_ context.Context, h Hook) error {
		*ran = append(*ran, h.Cmd[0])
		if slices.Contains(failing, h.Cmd[0]) {
			return errors.New("exit 1")
		}
		return nil
	}
	return bracket, ran, alerts
}

func hook(name string) Hook        { return Hook{Cmd: []string{name}} }
func ignoredHook(name string) Hook { return Hook{Cmd: []string{name}, Ignore: true} }
func presetHook(name string) Hook  { return Hook{Cmd: []string{name}, Ignore: true, preset: true} }

// The audit's case: maintenance on succeeds, a custom pre-hook fails, and
// maintenance off used to be left unarmed.
func TestAFailedPreHookStillGetsItsCompensation(t *testing.T) {
	b, ran, _ := hookFixture(t, []Hook{presetHook("maintenance-on"), hook("custom-pre")}, []Hook{presetHook("maintenance-off")}, "custom-pre")

	if err := b.runPre(context.Background()); err == nil {
		t.Fatal("a failing custom pre-hook must fail the backup")
	}
	b.release()

	if want := []string{"maintenance-on", "custom-pre", "maintenance-off"}; !slices.Equal(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
}

// A hook may change state and then exit nonzero, so even the first one failing
// leaves something to undo.
func TestTheFirstPreHookFailingStillGetsItsCompensation(t *testing.T) {
	b, ran, _ := hookFixture(t, []Hook{hook("pre")}, []Hook{hook("post")}, "pre")

	_ = b.runPre(context.Background())
	b.release()

	if !slices.Contains(*ran, "post") {
		t.Errorf("ran %v", *ran)
	}
}

func TestReleaseRunsOnce(t *testing.T) {
	b, ran, _ := hookFixture(t, []Hook{hook("pre")}, []Hook{hook("post")})

	_ = b.runPre(context.Background())
	b.release()
	b.release()

	if want := []string{"pre", "post"}; !slices.Equal(*ran, want) {
		t.Errorf("ran %v, want %v", *ran, want)
	}
}

// Post-hooks with no pre-hook have never run, and nothing has been opened to close.
func TestNoPreHooksMeansNoWindow(t *testing.T) {
	b, ran, _ := hookFixture(t, nil, []Hook{hook("post")})

	_ = b.runPre(context.Background())
	b.release()

	if len(*ran) != 0 {
		t.Errorf("ran %v", *ran)
	}
}

// A canceled backup must still close its window: the release has its own context.
func TestReleaseSurvivesACanceledBackup(t *testing.T) {
	b, ran, _ := hookFixture(t, []Hook{hook("pre")}, []Hook{hook("post")})
	var releaseErr error
	exec := b.exec
	b.exec = func(ctx context.Context, h Hook) error {
		if h.Cmd[0] == "post" {
			releaseErr = ctx.Err()
		}
		return exec(ctx, h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = b.runPre(ctx)
	cancel()

	b.release()

	if releaseErr != nil || !slices.Contains(*ran, "post") {
		t.Errorf("ran %v, context error %v", *ran, releaseErr)
	}
}

func TestAFailedPostHookRaisesAnAlert(t *testing.T) {
	b, _, alerts := hookFixture(t, []Hook{hook("pre")}, []Hook{ignoredHook("post")}, "post")

	_ = b.runPre(context.Background())
	b.release()

	if len(*alerts) != 1 || !strings.Contains((*alerts)[0], "app") {
		t.Errorf("alerts = %v", *alerts)
	}
}

// The built-in Nextcloud hooks also run on the app's cron container, where both
// halves fail every time. That is not news on every backup.
func TestAPresetThatDoesNotApplyStaysQuiet(t *testing.T) {
	b, ran, alerts := hookFixture(t, []Hook{presetHook("on")}, []Hook{presetHook("off")}, "on", "off")

	if err := b.runPre(context.Background()); err != nil {
		t.Fatalf("an ignored hook must not fail the backup: %v", err)
	}
	b.release()

	if len(*alerts) != 0 {
		t.Errorf("alerts = %v", *alerts)
	}
	if !slices.Contains(*ran, "off") {
		t.Errorf("ran %v", *ran)
	}

	// Its pre-hook worked, so its post-hook failing is real.
	b2, _, alerts2 := hookFixture(t, []Hook{presetHook("on")}, []Hook{presetHook("off")}, "off")
	_ = b2.runPre(context.Background())
	b2.release()
	if len(*alerts2) != 1 {
		t.Errorf("alerts = %v", *alerts2)
	}
}

func TestTheWindowIsJournaledUntilReleased(t *testing.T) {
	b, _, _ := hookFixture(t, []Hook{hook("pre")}, []Hook{hook("post")})

	_ = b.runPre(context.Background())
	rows, err := b.e.Store.ListWorkloadHolds()
	if err != nil || len(rows) != 1 || rows[0].Kind != holdKindHooks || rows[0].Detail != "image:1" {
		t.Fatalf("rows = %+v, err %v", rows, err)
	}
	b.release()
	if rows, _ := b.e.Store.ListWorkloadHolds(); len(rows) != 0 {
		t.Errorf("rows = %+v", rows)
	}
}

// The other half of the audit's complaint. There is no runtime symptom short of a
// slow upload, so the order in Run is what is pinned.
func TestTheWindowClosesBeforeTheUploadTail(t *testing.T) {
	src, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatal(err)
	}
	body := functionBody(t, string(src), "func (e *Engine) Run(")
	release := strings.LastIndex(body, "hooks.release()")
	tail := strings.Index(body, "e.storeAndVerify(")
	if release < 0 || tail < 0 || release > tail {
		t.Errorf("the last release (%d) must come before the upload tail (%d)", release, tail)
	}
	resume := strings.Index(body, "doResume()\n\t\thooks.release()")
	if resume < 0 {
		t.Error("the release must follow the resume, since a paused container cannot exec")
	}
}
