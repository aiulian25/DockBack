package dockercli

import (
	"encoding/json"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

func inspectWithPolicy(t *testing.T, name string, retries int) []byte {
	t.Helper()
	raw, err := json.Marshal(types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			HostConfig: &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(name), MaximumRetryCount: retries},
			},
		},
		Config: &container.Config{Image: "wikijs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func policyOf(t *testing.T, raw []byte) (string, int) {
	t.Helper()
	var insp types.ContainerJSON
	if err := json.Unmarshal(raw, &insp); err != nil || insp.HostConfig == nil {
		t.Fatalf("unreadable: %v", err)
	}
	return string(insp.HostConfig.RestartPolicy.Name), insp.HostConfig.RestartPolicy.MaximumRetryCount
}

func TestPromoteRestartPolicy(t *testing.T) {
	t.Run("R1 §8's on-failure:5 is promoted, and the retry count goes with it", func(t *testing.T) {
		// MaximumRetryCount is only meaningful for on-failure, and Docker rejects
		// a non-zero count on any other policy.
		out, changed, err := PromoteRestartPolicy(inspectWithPolicy(t, "on-failure", 5))
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		name, retries := policyOf(t, out)
		if name != BootSafePolicy {
			t.Errorf("policy = %q, want %q", name, BootSafePolicy)
		}
		if retries != 0 {
			t.Errorf("retry count = %d, want 0", retries)
		}
	})

	t.Run("an unset policy is promoted too", func(t *testing.T) {
		// Docker's default is `no`, so a container with none set is exactly the
		// one that disappears after a reboot.
		out, changed, err := PromoteRestartPolicy(inspectWithPolicy(t, "", 0))
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		if name, _ := policyOf(t, out); name != BootSafePolicy {
			t.Errorf("policy = %q", name)
		}
	})

	t.Run("an explicit no is promoted", func(t *testing.T) {
		_, changed, _ := PromoteRestartPolicy(inspectWithPolicy(t, "no", 0))
		if !changed {
			t.Error("`no` does not survive a reboot either")
		}
	})

	t.Run("an already boot-safe policy is left byte-identical", func(t *testing.T) {
		// Choosing the option on a container that does not need it must not churn.
		for _, policy := range []string{"always", BootSafePolicy} {
			in := inspectWithPolicy(t, policy, 0)
			out, changed, err := PromoteRestartPolicy(in)
			if err != nil || changed {
				t.Fatalf("%s: changed=%v err=%v", policy, changed, err)
			}
			if string(out) != string(in) {
				t.Errorf("%s: document changed", policy)
			}
		}
	})

	t.Run("which policies actually survive a reboot", func(t *testing.T) {
		for _, safe := range []string{"always", "unless-stopped", "UNLESS-STOPPED", " always "} {
			if !RestartPolicyBootSafe(safe) {
				t.Errorf("%q starts with the daemon", safe)
			}
		}
		for _, unsafe := range []string{"", "no", "on-failure", "on-failure:5"} {
			if RestartPolicyBootSafe(unsafe) {
				t.Errorf("%q does not start with the daemon", unsafe)
			}
		}
	})

	t.Run("the recorded policy can be read back for the message", func(t *testing.T) {
		if got := RestartPolicyName(inspectWithPolicy(t, "on-failure", 5)); got != "on-failure" {
			t.Errorf("name = %q", got)
		}
		if got := RestartPolicyName([]byte("not json")); got != "" {
			t.Errorf("unreadable must yield empty, got %q", got)
		}
	})

	t.Run("a malformed document is refused, not mangled", func(t *testing.T) {
		if _, _, err := PromoteRestartPolicy([]byte("{")); err == nil {
			t.Error("want an error")
		}
	})
}
