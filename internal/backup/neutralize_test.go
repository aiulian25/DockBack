package backup

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// mailPassword is the value that must never reach a log line. Shaped like the
// Gmail app password R2 §Issue 17 found in the clone's environment.
const mailPassword = "abcd efgh ijkl mnop"

// inspectWithEnv builds the minimal inspect document the restore rewrites.
func inspectWithEnv(env ...string) []byte {
	doc := map[string]any{"Config": map[string]any{"Env": env, "Image": "solidnerd/bookstack:23.06"}}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return raw
}

func appliedKeys(plan neutralizationPlan) []string {
	out := make([]string, 0, len(plan.Apply))
	for _, rule := range plan.Apply {
		out = append(out, rule.Key)
	}
	return out
}

func TestNeutralizeOnClone(t *testing.T) {
	t.Run("a profile rule wins over the generic name match", func(t *testing.T) {
		// Both tiers claim MAIL_HOST. The profile knows a safe VALUE; the generic
		// tier only knows how to empty things, and emptying a host an app expects
		// to be set is a different failure from pointing it somewhere inert.
		profile := &AppProfile{NeutralizeOnClone: []EnvNeutralization{
			{Key: "MAIL_HOST", Mode: NeutralizeSet, Value: "localhost", Why: "test"},
		}}
		plan := planNeutralizations(profile, []string{"MAIL_HOST=smtp.gmail.com"}, nil)

		if len(plan.Apply) != 1 {
			t.Fatalf("a key claimed by both tiers must be changed once: %+v", plan.Apply)
		}
		if plan.Apply[0].Mode != NeutralizeSet || plan.Apply[0].Value != "localhost" {
			t.Errorf("the profile's mode must win: %+v", plan.Apply[0])
		}
	})

	t.Run("a key the image bakes in is left alone and reported", func(t *testing.T) {
		profile := &AppProfile{NeutralizeOnClone: []EnvNeutralization{
			{Key: "MAIL_DRIVER", Mode: NeutralizeSet, Value: "log", Why: "test"},
		}}
		env := []string{"MAIL_DRIVER=smtp", "SMTP_HOST=smtp.example.com", "APP_URL=https://x"}
		plan := planNeutralizations(profile, env, []string{"MAIL_DRIVER", "PATH"})

		if slices.Contains(appliedKeys(plan), "MAIL_DRIVER") {
			t.Error("overriding an image's own setting is how a restore breaks the app it is copying")
		}
		if !slices.Contains(plan.Baked, "MAIL_DRIVER") {
			t.Errorf("a skipped key must be reported — the clone can still act through it: %v", plan.Baked)
		}
		// The generic tier is subject to the same rule, and a non-baked generic
		// match still goes through.
		if !slices.Contains(appliedKeys(plan), "SMTP_HOST") {
			t.Errorf("SMTP_HOST is not baked and must be cleared: %v", appliedKeys(plan))
		}
		bakedGeneric := planNeutralizations(nil, []string{"SMTP_HOST=x"}, []string{"SMTP_HOST"})
		if len(bakedGeneric.Apply) != 0 || !slices.Contains(bakedGeneric.Baked, "SMTP_HOST") {
			t.Errorf("a baked generic match must also be left alone: %+v", bakedGeneric)
		}
	})

	t.Run("the generic tier matches only unambiguous outbound names", func(t *testing.T) {
		env := []string{
			"SMTP_HOST=smtp.example.com", "MAIL_HOST=mail.example.com",
			"DISCORD_WEBHOOK_URL=https://discord.com/api/webhooks/x",
			"NTFY_PUSH_TOKEN=tk_live", "APPRISE_URL=json://x",
			"APP_URL=https://wiki.example.com", "DB_HOST=db", "PATH=/usr/bin",
		}
		plan := planNeutralizations(nil, env, nil)
		keys := appliedKeys(plan)

		for _, want := range []string{"SMTP_HOST", "MAIL_HOST", "DISCORD_WEBHOOK_URL", "NTFY_PUSH_TOKEN", "APPRISE_URL"} {
			if !slices.Contains(keys, want) {
				t.Errorf("%s reaches out and must be cleared: %v", want, keys)
			}
		}
		// APP_URL is where the app is SERVED, not somewhere it calls. Clearing it
		// would break the clone without making anything safer.
		for _, notWant := range []string{"APP_URL", "DB_HOST", "PATH"} {
			if slices.Contains(keys, notWant) {
				t.Errorf("%s is not an outbound integration: %v", notWant, keys)
			}
		}
		for _, rule := range plan.Apply {
			if rule.Mode != NeutralizeClear {
				t.Errorf("a generic match knows nothing about the app and may only empty: %+v", rule)
			}
		}
	})

	t.Run("a rule for a variable this deployment does not set adds nothing", func(t *testing.T) {
		// Otherwise a clone would carry a variable the original never had.
		profile := &AppProfile{NeutralizeOnClone: []EnvNeutralization{
			{Key: "MAIL_PASSWORD", Mode: NeutralizeClear, Why: "test"},
		}}
		plan := planNeutralizations(profile, []string{"APP_URL=https://x"}, nil)
		if len(plan.Apply) != 0 || len(plan.Baked) != 0 {
			t.Errorf("nothing to neutralise: %+v", plan)
		}
	})

	t.Run("an in-place restore is untouched, and a clone reports exactly what changed", func(t *testing.T) {
		env := []string{
			"MAIL_DRIVER=smtp",
			"MAIL_PASSWORD=" + mailPassword,
			"SMTP_HOST=smtp.gmail.com",
			"APP_URL=https://wiki.example.com",
		}
		man := &Manifest{Image: "solidnerd/bookstack:23.06", ImageConfig: &ImageConfig{EnvKeys: []string{"PATH"}}}
		b := &store.Backup{ID: "run1", TargetName: "bookstack"}

		// A cutover must keep its credentials, or the thing it restored does not work.
		var inPlaceLogs []string
		e := &Engine{Log: func(_, _, msg string) { inPlaceLogs = append(inPlaceLogs, msg) }}
		before := inspectWithEnv(env...)
		after := e.applyCloneNeutralizations(b, man, before, false)
		if string(after) != string(before) {
			t.Error("an in-place restore must keep every value it recorded")
		}
		if len(inPlaceLogs) != 0 {
			t.Errorf("and must say nothing about neutralising: %v", inPlaceLogs)
		}

		var logs []string
		e = &Engine{Log: func(_, _, msg string) { logs = append(logs, msg) }}
		cloned := e.applyCloneNeutralizations(b, man, inspectWithEnv(env...), true)

		got := envValues(dockercli.ContainerEnv(cloned))
		if got["MAIL_DRIVER"] != "log" {
			t.Errorf("MAIL_DRIVER = %q, want log — the clone must compose mail without delivering it", got["MAIL_DRIVER"])
		}
		if got["MAIL_PASSWORD"] != "" {
			t.Error("the clone must not be able to authenticate to the real mail account")
		}
		if got["SMTP_HOST"] != "" {
			t.Error("SMTP_HOST must be cleared by the generic tier")
		}
		if got["APP_URL"] != "https://wiki.example.com" {
			t.Errorf("APP_URL = %q — a clone that cannot serve itself proves nothing", got["APP_URL"])
		}

		block := strings.Join(logs, "\n")
		for _, want := range []string{"Neutralised for this clone", "MAIL_DRIVER (log)", "MAIL_PASSWORD (cleared)", "SMTP_HOST (cleared)", "re-arm by restoring in place"} {
			if !strings.Contains(block, want) {
				t.Errorf("the report block must contain %q: %s", want, block)
			}
		}
		if strings.Contains(block, "APP_URL") {
			t.Errorf("the block must list exactly what changed: %s", block)
		}
		// The block says what a variable BECAME, never what it was.
		if strings.Contains(block, mailPassword) {
			t.Errorf("a credential VALUE reached the run log: %s", block)
		}
	})
}
