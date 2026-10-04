package api

import (
	"strings"
	"testing"

	"dockback/internal/backup"
)

// A cross-host restore of a hardware-transcoding container onto a host with no
// GPU used to be created and then fail at start with a raw Docker error — after
// the data had already been written. These tests pin what the preflight says,
// and the distinction that makes it trustworthy: a statement of FACT ("not
// present on X") must never be produced from a failed probe.

func TestPortabilityWarningsMissingDevice(t *testing.T) {
	req := &backup.HostRequirements{Devices: []string{"/dev/dri/renderD128"}}
	facts := hostFacts{Target: "nuc", DevKnown: true, Devices: map[string]bool{"/dev/sda": true}}

	got := portabilityWarnings(req, facts)
	if len(got) != 1 {
		t.Fatalf("want exactly one warning, got %v", got)
	}
	for _, want := range []string{"/dev/dri/renderD128", "not present on", "nuc"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("the warning must name %q: %s", want, got[0])
		}
	}
}

// A device the target HAS must be silent. Warning about something that is fine
// trains the operator to skip the list.
func TestPortabilityWarningsPresentDeviceIsSilent(t *testing.T) {
	req := &backup.HostRequirements{Devices: []string{"/dev/dri/renderD128"}}
	facts := hostFacts{
		Target: "razer", DevKnown: true,
		Devices: map[string]bool{"/dev/dri/renderD128": true},
	}
	if got := portabilityWarnings(req, facts); len(got) != 0 {
		t.Fatalf("a device that is present must produce no warning: %v", got)
	}
}

// THE property that makes this trustworthy: an unprobed host must never yield a
// confident "not present". "I could not check" and "it is missing" are different
// claims, and only one of them is a fact.
func TestPortabilityWarningsUnverifiableNeverAsserts(t *testing.T) {
	req := &backup.HostRequirements{Devices: []string{"/dev/dri/renderD128"}, GPUs: 1}
	facts := hostFacts{Target: "ds920"} // DevKnown false — nothing was read

	got := portabilityWarnings(req, facts)
	if len(got) != 2 {
		t.Fatalf("want a warning per unverifiable requirement, got %v", got)
	}
	for _, w := range got {
		if !strings.Contains(w, "could not verify") {
			t.Fatalf("an unprobed host must say so: %s", w)
		}
		if strings.Contains(w, "not present on") {
			t.Fatalf("an unprobed host must NEVER assert absence: %s", w)
		}
	}
}

func TestPortabilityWarningsGPU(t *testing.T) {
	req := &backup.HostRequirements{GPUs: 1}

	// No GPU at all on a host we DID read: a statement of fact.
	got := portabilityWarnings(req, hostFacts{Target: "nuc", DevKnown: true, Devices: map[string]bool{}})
	if len(got) != 1 || !strings.Contains(got[0], "no GPU device is present") {
		t.Fatalf("a GPU-less target must be named: %v", got)
	}

	// A GPU present is NOT the whole answer — `--gpus` also needs the NVIDIA
	// container runtime, and silence here would be misleading.
	got = portabilityWarnings(req, hostFacts{
		Target: "razer", DevKnown: true, HasGPU: true,
		Devices: map[string]bool{"/dev/nvidia0": true},
	})
	if len(got) != 1 || !strings.Contains(got[0], "container runtime") {
		t.Fatalf("a present GPU must still flag the runtime requirement: %v", got)
	}
}

func TestPortabilityWarningsLogDriver(t *testing.T) {
	req := &backup.HostRequirements{LogDriver: "syslog"}

	// Matching driver → silent.
	if got := portabilityWarnings(req, hostFacts{Target: "nuc", LogDriver: "syslog"}); len(got) != 0 {
		t.Fatalf("a matching log driver must be silent: %v", got)
	}
	// Different driver → named, with both values.
	got := portabilityWarnings(req, hostFacts{Target: "nuc", LogDriver: "json-file"})
	if len(got) != 1 || !strings.Contains(got[0], "syslog") || !strings.Contains(got[0], "json-file") {
		t.Fatalf("both drivers must be named: %v", got)
	}
	// Unknown driver → "could not verify", never an assertion.
	got = portabilityWarnings(req, hostFacts{Target: "nuc"})
	if len(got) != 1 || !strings.Contains(got[0], "could not verify") {
		t.Fatalf("an unknown driver must not be asserted against: %v", got)
	}
}

// Privileged, capabilities and sysctls cannot be probed remotely — only the
// target can really answer. They are reported once, plainly, rather than as
// "could not verify" noise.
func TestPortabilityWarningsUnprobableRequirements(t *testing.T) {
	req := &backup.HostRequirements{
		Privileged: true,
		CapAdd:     []string{"NET_ADMIN", "SYS_PTRACE"},
		Sysctls:    map[string]string{"net.core.somaxconn": "1024", "net.ipv4.ip_forward": "1"},
	}
	got := portabilityWarnings(req, hostFacts{Target: "nuc", DevKnown: true, Devices: map[string]bool{}})
	if len(got) != 3 {
		t.Fatalf("want one warning each for privileged/caps/sysctls, got %v", got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"PRIVILEGED", "NET_ADMIN", "SYS_PTRACE", "net.core.somaxconn", "net.ipv4.ip_forward"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the warnings must name %q:\n%s", want, joined)
		}
	}
	// Sysctl KEYS are listed, never their values — a value can be tuning detail
	// nobody needs in a dialog, and the key is what identifies the requirement.
	if strings.Contains(joined, "1024") {
		t.Fatalf("sysctl values must not be printed:\n%s", joined)
	}
}

// An ordinary container asks nothing of its host, so a cross-host restore of it
// must show no dialog noise at all.
func TestPortabilityWarningsOrdinaryContainerIsSilent(t *testing.T) {
	if got := portabilityWarnings(nil, hostFacts{Target: "nuc"}); got != nil {
		t.Fatalf("a container with no requirements must produce nothing: %v", got)
	}
	if got := portabilityWarnings(&backup.HostRequirements{}, hostFacts{Target: "nuc", DevKnown: true}); len(got) != 0 {
		t.Fatalf("empty requirements must produce nothing: %v", got)
	}
}

// Step 27: only a device the target VERIFIABLY lacks stops a restore; one that
// could not be checked does not, because that is not a fact.
func TestMissingDevicesIsOnlyWhatIsKnownMissing(t *testing.T) {
	req := &backup.HostRequirements{Devices: []string{"/dev/net/tun", "/dev/dri/renderD128"}}
	known := hostFacts{DevKnown: true, Devices: map[string]bool{"/dev/dri/renderD128": true}}
	if got := missingDevices(req, known); len(got) != 1 || got[0] != "/dev/net/tun" {
		t.Errorf("gluetun's tun device is missing, the GPU is there: %v", got)
	}
	if got := missingDevices(req, hostFacts{DevKnown: false}); got != nil {
		t.Errorf("an unreadable device list blocks nothing: %v", got)
	}
	if got := missingDevices(nil, known); got != nil {
		t.Errorf("no requirements, nothing missing: %v", got)
	}
}

// Step 27: a network the stack owns is recreated as recorded; one it only joins
// must already exist on the target, or the container is cut off.
func TestSharedNetworksMissing(t *testing.T) {
	man := &backup.Manifest{Networks: []backup.NetworkRef{
		{Name: "arr_default", Labels: map[string]string{"com.docker.compose.project": "arr"}},
		{Name: "npm"},
		{Name: "newt"},
		{Name: "other_shared", Labels: map[string]string{"com.docker.compose.project": "other"}},
	}}
	got := sharedNetworksMissing(man, "arr", map[string]bool{"newt": true})
	if strings.Join(got, ",") != "npm,other_shared" {
		t.Errorf("only joined networks missing on the target, never the stack's own: %v", got)
	}
	if got := sharedNetworksMissing(man, "arr", map[string]bool{"npm": true, "newt": true, "other_shared": true}); len(got) != 0 {
		t.Errorf("everything joined exists, nothing to stop for: %v", got)
	}
}
