package dockercli

import (
	"strings"
	"testing"
)

// nvidia-smi is the ONLY source of NVIDIA GPU telemetry: the proprietary driver
// registers no hwmon node, which is why lm-sensors can't read it either. These
// tests pin the parsing and — more importantly — that "[N/A]" never becomes a
// zero, because "no fan sensor" and "fan stopped" are different facts.

func TestParseNvidiaSMI(t *testing.T) {
	out := "00000000:01:00.0, NVIDIA GeForce RTX 3060 Laptop GPU, 64, 37, 6144, 1024, 45.32, 55\n"
	got := ParseNvidiaSMI(out, nvidiaQueries[0])
	if len(got) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(got))
	}
	g := got[0]
	if g.Name != "NVIDIA GeForce RTX 3060 Laptop GPU" {
		t.Fatalf("name = %q", g.Name)
	}
	if g.TempC != 64 || g.UsagePct != 37 || g.PowerW != 45.32 || g.FanPct != 55 {
		t.Fatalf("telemetry = %+v", g)
	}
	// memory.total/used arrive in MiB with --format=nounits.
	if g.MemTotal != 6144*1024*1024 || g.MemUsed != 1024*1024*1024 {
		t.Fatalf("vram = %d/%d bytes", g.MemUsed, g.MemTotal)
	}
}

// A laptop GPU with no controllable fan reports [N/A]. Storing 0 would claim the
// fan is stopped.
func TestParseNvidiaSMINotAvailable(t *testing.T) {
	out := "00000000:01:00.0, NVIDIA T400, 41, [N/A], 4096, 120, [Not Supported], [N/A]\n"
	g := ParseNvidiaSMI(out, nvidiaQueries[0])[0]
	if g.TempC != 41 {
		t.Fatalf("a real value beside N/A ones must still parse: %v", g.TempC)
	}
	if g.UsagePct != 0 || g.PowerW != 0 || g.FanPct != 0 {
		t.Fatalf("[N/A] must read as absent (zero), got %+v", g)
	}
}

func TestParseNvidiaSMIMultipleAndGarbage(t *testing.T) {
	out := "00000000:01:00.0, A, 40, 10, 100, 10, 5, 20\n" +
		"00000000:C1:00.0, B, 50, 20, 200, 20, 6, 30\n" +
		"\nnot,enough,fields\n"
	got := ParseNvidiaSMI(out, nvidiaQueries[0])
	if len(got) != 2 {
		t.Fatalf("want 2 GPUs (garbage skipped), got %d", len(got))
	}
	if got[1].BusID != "00000000:C1:00.0" {
		t.Fatalf("bus id = %q", got[1].BusID)
	}
	if len(ParseNvidiaSMI("", nvidiaQueries[0])) != 0 {
		t.Fatal("empty output must yield no GPUs")
	}
}

// nvidia-smi prints an 8-digit PCI domain, sysfs uses 4. A naive compare never
// matches, which would silently drop every reading.
func TestMergeNvidiaStatsMatchesByBusID(t *testing.T) {
	gpus := []MachineGPU{
		{Card: "card0", VendorID: "0x8086", DeviceID: "0x9a60", Driver: "i915", PCIAddr: "0000:00:02.0"},
		{Card: "card1", VendorID: "0x10de", DeviceID: "0x2560", Driver: "nvidia", PCIAddr: "0000:01:00.0"},
	}
	MergeNvidiaStats(gpus, []NvidiaStat{{
		BusID: "00000000:01:00.0", Name: "NVIDIA GeForce RTX 3060 Laptop GPU",
		TempC: 64, UsagePct: 37, MemTotal: 6 << 30, MemUsed: 1 << 30, PowerW: 45, FanPct: 55,
	}})

	// The integrated card must be untouched — attributing the discrete GPU's
	// temperature to the iGPU is exactly what index-order matching would do.
	if gpus[0].TempC != 0 || gpus[0].Telemetry != "" {
		t.Fatalf("the iGPU must not receive the dGPU's telemetry: %+v", gpus[0])
	}
	g := gpus[1]
	if g.TempC != 64 || g.UsagePct != 37 || g.FanPct != 55 || g.PowerW != 45 {
		t.Fatalf("telemetry did not land: %+v", g)
	}
	if g.VRAMTotal != 6<<30 || g.VRAMUsed != 1<<30 {
		t.Fatalf("vram = %d/%d", g.VRAMUsed, g.VRAMTotal)
	}
	if g.Name != "NVIDIA GeForce RTX 3060 Laptop GPU" {
		t.Fatalf("nvidia-smi's name must win: %q", g.Name)
	}
	if g.Telemetry != "nvidia-smi" {
		t.Fatalf("telemetry source = %q", g.Telemetry)
	}
}

// A stat for a card sysfs never saw must be dropped, not applied to whatever
// happens to be first.
func TestMergeNvidiaStatsUnknownBusIsIgnored(t *testing.T) {
	gpus := []MachineGPU{{Card: "card0", PCIAddr: "0000:01:00.0"}}
	MergeNvidiaStats(gpus, []NvidiaStat{{BusID: "00000000:99:00.0", TempC: 90}})
	if gpus[0].TempC != 0 {
		t.Fatalf("an unmatched stat must not be applied: %+v", gpus[0])
	}
}

func TestHasNvidia(t *testing.T) {
	if HasNvidia([]MachineGPU{{VendorID: "0x8086"}}) {
		t.Fatal("an Intel-only machine must not trigger the nvidia probe")
	}
	if !HasNvidia([]MachineGPU{{VendorID: "0x8086"}, {VendorID: "0x10DE"}}) {
		t.Fatal("vendor id matching must be case-insensitive")
	}
}

// The exact shape a real RTX 2060 laptop card returns: an idle GPU (0 %), a
// mostly-empty framebuffer, and no controllable fan.
func TestParseNvidiaSMIRealIdleLaptopCard(t *testing.T) {
	// pci.bus_id, name, temperature.gpu, utilization.gpu, memory.total,
	// memory.used, power.draw, fan.speed
	out := "00000000:01:00.0, NVIDIA GeForce RTX 2060, 50, 0, 6144, 4, 6.00, [N/A]\n"
	g := ParseNvidiaSMI(out, nvidiaQueries[0])[0]

	if g.Name != "NVIDIA GeForce RTX 2060" || g.TempC != 50 {
		t.Fatalf("identity/temp: %+v", g)
	}
	if g.PowerW != 6 {
		t.Fatalf("power = %v W, want 6", g.PowerW)
	}
	if g.MemTotal != 6144*1024*1024 || g.MemUsed != 4*1024*1024 {
		t.Fatalf("vram = %d/%d bytes", g.MemUsed, g.MemTotal)
	}
	if g.FanPct != 0 {
		t.Fatalf("an [N/A] fan must stay absent, got %d", g.FanPct)
	}

	// An IDLE gpu (0 % utilisation) must still count as having telemetry — the
	// card is being read successfully, it simply has nothing to do.
	gpus := []MachineGPU{{Card: "card0", VendorID: "0x10de", Driver: "nvidia", PCIAddr: "0000:01:00.0"}}
	MergeNvidiaStats(gpus, []NvidiaStat{g})
	if gpus[0].Telemetry != "nvidia-smi" {
		t.Fatalf("an idle GPU must still be marked as read: %+v", gpus[0])
	}
	if gpus[0].TempC != 50 || gpus[0].VRAMTotal == 0 {
		t.Fatalf("telemetry did not land on an idle card: %+v", gpus[0])
	}
}

// Some driver builds print the markers without brackets.
func TestParseNvidiaSMIBareNotAvailable(t *testing.T) {
	out := "00000000:01:00.0, NVIDIA T400, 41, N/A, 4096, 120, Not Supported, N/A\n"
	g := ParseNvidiaSMI(out, nvidiaQueries[0])[0]
	if g.TempC != 41 {
		t.Fatalf("temp = %v", g.TempC)
	}
	if g.UsagePct != 0 || g.PowerW != 0 || g.FanPct != 0 {
		t.Fatalf("bare N/A markers must read as absent: %+v", g)
	}
}

// The probe must work across the whole NVIDIA range, not one laptop card. These
// are the shapes that actually differ in the field: reduced queries on older
// drivers, passive datacentre cards with no fan, multi-GPU servers, multi-domain
// PCI, and names containing a comma.

// A reduced fallback query must parse correctly — the parser reads columns by
// the field name it asked for, not by a fixed position.
func TestParseNvidiaSMIReducedQuerySets(t *testing.T) {
	cases := []struct {
		name   string
		fields []string
		line   string
		want   NvidiaStat
	}{
		{
			name:   "no fan (passive Tesla / datacentre card)",
			fields: nvidiaQueries[1], // ...power.draw, no fan.speed
			line:   "00000000:3B:00.0, Tesla V100-SXM2-16GB, 38, 0, 16160, 0, 42.15",
			want:   NvidiaStat{TempC: 38, MemTotal: 16160 << 20, PowerW: 42.15},
		},
		{
			name:   "no power telemetry (older vGPU)",
			fields: nvidiaQueries[2],
			line:   "00000000:00:08.0, GRID M60-2Q, 45, 12, 2048, 512",
			want:   NvidiaStat{TempC: 45, UsagePct: 12, MemTotal: 2048 << 20, MemUsed: 512 << 20},
		},
		{
			name:   "no utilisation (very old driver)",
			fields: nvidiaQueries[3],
			line:   "00000000:01:00.0, Quadro K2200, 52, 4096, 300",
			want:   NvidiaStat{TempC: 52, MemTotal: 4096 << 20, MemUsed: 300 << 20},
		},
		{
			name:   "identity only (minimum viable)",
			fields: nvidiaQueries[5],
			line:   "00000000:01:00.0, NVIDIA GeForce GT 710",
			want:   NvidiaStat{},
		},
	}
	for _, tc := range cases {
		got := ParseNvidiaSMI(tc.line+"\n", tc.fields)
		if len(got) != 1 {
			t.Fatalf("%s: want 1 GPU, got %d", tc.name, len(got))
		}
		g := got[0]
		if g.TempC != tc.want.TempC || g.UsagePct != tc.want.UsagePct {
			t.Errorf("%s: temp/util = %v/%v, want %v/%v", tc.name, g.TempC, g.UsagePct, tc.want.TempC, tc.want.UsagePct)
		}
		if g.MemTotal != tc.want.MemTotal || g.PowerW != tc.want.PowerW {
			t.Errorf("%s: mem/power = %d/%v, want %d/%v", tc.name, g.MemTotal, g.PowerW, tc.want.MemTotal, tc.want.PowerW)
		}
		if g.Name == "" || strings.Contains(g.Name, ",") {
			t.Errorf("%s: name did not reassemble cleanly: %q", tc.name, g.Name)
		}
	}
}

// Some OEM builds report a name containing a comma. A naive comma split shreds
// it and shifts every numeric column by one.
func TestParseNvidiaSMINameWithComma(t *testing.T) {
	out := "00000000:01:00.0, NVIDIA RTX A4000, Laptop GPU, 61, 22, 8192, 1024, 35.5, 40\n"
	g := ParseNvidiaSMI(out, nvidiaQueries[0])
	if len(g) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(g))
	}
	if g[0].Name != "NVIDIA RTX A4000, Laptop GPU" {
		t.Fatalf("name = %q", g[0].Name)
	}
	// The point of the test: the numbers must not have shifted.
	if g[0].TempC != 61 || g[0].UsagePct != 22 || g[0].FanPct != 40 {
		t.Fatalf("columns shifted: %+v", g[0])
	}
}

// A multi-GPU server: every card must get its OWN numbers, matched by bus id.
func TestMergeNvidiaStatsMultiGPUServer(t *testing.T) {
	gpus := []MachineGPU{
		{Card: "card0", VendorID: "0x10de", Driver: "nvidia", PCIAddr: "0000:3b:00.0"},
		{Card: "card1", VendorID: "0x10de", Driver: "nvidia", PCIAddr: "0000:5e:00.0"},
		{Card: "card2", VendorID: "0x10de", Driver: "nvidia", PCIAddr: "0000:af:00.0"},
	}
	MergeNvidiaStats(gpus, []NvidiaStat{
		{BusID: "00000000:AF:00.0", TempC: 71, UsagePct: 99},
		{BusID: "00000000:3B:00.0", TempC: 38, UsagePct: 0},
		{BusID: "00000000:5E:00.0", TempC: 55, UsagePct: 47},
	})
	want := map[string]float64{"card0": 38, "card1": 55, "card2": 71}
	for _, g := range gpus {
		if g.TempC != want[g.Card] {
			t.Fatalf("%s got %v °C, want %v — stats matched by order rather than bus id",
				g.Card, g.TempC, want[g.Card])
		}
	}
	// An idle card (0 %) must still record its utilisation, not be skipped.
	if gpus[0].Telemetry != "nvidia-smi" || gpus[0].UsagePct != 0 {
		t.Fatalf("idle card mis-handled: %+v", gpus[0])
	}
}

// Large servers really do have multiple PCI domains. Discarding the domain would
// make two distinct GPUs look like the same card.
func TestNormalizeBusIDKeepsDomain(t *testing.T) {
	if normalizeBusID("00000000:01:00.0") != normalizeBusID("0000:01:00.0") {
		t.Fatal("an 8-digit and a 4-digit domain for the same address must match")
	}
	if normalizeBusID("00000001:01:00.0") == normalizeBusID("00000000:01:00.0") {
		t.Fatal("different PCI domains must NOT be treated as the same card")
	}
	if normalizeBusID("00000000:AF:00.0") != normalizeBusID("0000:af:00.0") {
		t.Fatal("bus id matching must be case-insensitive")
	}
}

// Every query set must start with the two fields the parser relies on.
func TestNvidiaQuerySetsAreWellFormed(t *testing.T) {
	for i, q := range nvidiaQueries {
		if len(q) < 2 || q[0] != "pci.bus_id" || q[1] != "name" {
			t.Fatalf("query set %d must begin with pci.bus_id,name: %v", i, q)
		}
		if i > 0 && len(q) >= len(nvidiaQueries[i-1]) {
			t.Fatalf("query set %d is not narrower than the one before it", i)
		}
	}
}
