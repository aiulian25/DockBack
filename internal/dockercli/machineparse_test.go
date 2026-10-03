package dockercli

import (
	"encoding/json"
	"strings"
	"testing"
)

// The probe runs against wildly different hosts — a laptop with thermal zones, a
// VM with no DMI at all, a NAS whose firmware fills DMI with placeholders. These
// tests pin the rule that governs all of it: absence is never an error and never
// a zero, and a rate is never negative.

// laptopProbe is a realistic capture: full DMI, cpufreq, two GPUs, an NVMe, a
// wireless NIC, thermal zones and a battery.
const laptopProbe = "id.vendor\tRazer Inc.\n" +
	"id.product\tBlade 15 Advanced Model\n" +
	"id.board\tCH530\n" +
	"id.bios_version\t1.04\n" +
	"id.bios_date\t06/18/2021\n" +
	"id.chassis\t10\n" +
	"cpu.model\tIntel(R) Core(TM) i7-11800H @ 2.30GHz\n" +
	"cpu.cache\t24576 KB\n" +
	"cpu.threads\t16\n" +
	"cpu.cores\t8\n" +
	"cpu.mhz_min\t800000\n" +
	"cpu.mhz_max\t4600000\n" +
	"cpu.mhz_cur\t3810000\n" +
	"cpu.load\t0.42 0.55 0.61 1/1180 44213\n" +
	"sys.uptime\t1234567.89 9876543.21\n" +
	"mem.MemTotal\t65536000\n" +
	"mem.MemAvailable\t63000000\n" +
	"mem.Cached\t8800000\n" +
	"mem.SwapTotal\t2097152\n" +
	"mem.SwapFree\t2097152\n" +
	"gpu\tcard0|0x10de|0x2560|nvidia|\n" +
	"gpu\tcard1|0x8086|0x9a60|i915|\n" +
	"disk\tnvme0n1|1953525168|0\n" +
	"disk.model\tnvme0n1|Samsung SSD 980 PRO 1TB\n" +
	"net\twlan0|1200|up\n" +
	"therm\tx86_pkg_temp|48000\n" +
	"therm\tacpitz|45500\n" +
	"battery\t100\n" +
	"ac\t1\n" +
	"os.name\tUbuntu 24.04.1 LTS\n" +
	"os.kernel\t6.8.0-51-generic\n" +
	"a.cpu\t1000\t9000\n" +
	"a.disk\tnvme0n1\t1000\t2000\n" +
	"a.net\twlan0\t500000\t100000\n" +
	"b.cpu\t1050\t9450\n" +
	"b.disk\tnvme0n1\t5096\t30000\n" +
	"b.net\twlan0\t1700000\t480000\n"

func TestParseMachineProbeLaptop(t *testing.T) {
	m := ParseMachineProbe(laptopProbe)

	if m.Identity.Vendor != "Razer Inc." || m.Identity.Product != "Blade 15 Advanced Model" {
		t.Fatalf("identity: %+v", m.Identity)
	}
	if m.Identity.ChassisType != "Notebook" {
		t.Fatalf("chassis type 10 must resolve to Notebook, got %q", m.Identity.ChassisType)
	}
	if m.Identity.BIOSDate != "06/18/2021" {
		t.Fatalf("bios date: %q", m.Identity.BIOSDate)
	}
	if m.Identity.Virtualized {
		t.Fatal("a Razer laptop must not be flagged virtual")
	}

	if m.CPU.Cores != 8 || m.CPU.Threads != 16 {
		t.Fatalf("cpu topology = %d/%d, want 8/16", m.CPU.Cores, m.CPU.Threads)
	}
	if m.CPU.CacheKB != 24576 {
		t.Fatalf("cache = %d KB, want 24576", m.CPU.CacheKB)
	}
	// cpufreq is kHz; the UI wants MHz.
	if m.CPU.MHzNow != 3810 || m.CPU.MHzMax != 4600 || m.CPU.MHzMin != 800 {
		t.Fatalf("clocks = %v/%v/%v MHz, want 800/3810/4600", m.CPU.MHzMin, m.CPU.MHzNow, m.CPU.MHzMax)
	}
	if m.CPU.Load1 != 0.42 || m.CPU.Load15 != 0.61 {
		t.Fatalf("load = %v %v", m.CPU.Load1, m.CPU.Load15)
	}
	// busy delta 50, idle delta 450 => 10%.
	if m.CPU.UsagePct != 10 {
		t.Fatalf("cpu usage = %v%%, want 10", m.CPU.UsagePct)
	}

	// Used must be total-AVAILABLE, not total-free: counting reclaimable cache as
	// used is how a healthy Linux box gets misreported as out of memory.
	wantUsed := int64(65536000-63000000) * 1024
	if m.Memory.UsedBytes != wantUsed {
		t.Fatalf("used = %d, want %d (total - available)", m.Memory.UsedBytes, wantUsed)
	}
	if m.Memory.SwapUsedBytes != 0 {
		t.Fatalf("swap used = %d, want 0", m.Memory.SwapUsedBytes)
	}

	if len(m.GPUs) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(m.GPUs))
	}
	if m.GPUs[0].Vendor != "NVIDIA" || m.GPUs[0].Driver != "nvidia" {
		t.Fatalf("gpu0 = %+v", m.GPUs[0])
	}
	if m.GPUs[1].Vendor != "Intel" {
		t.Fatalf("gpu1 = %+v", m.GPUs[1])
	}

	if len(m.Disks) != 1 {
		t.Fatalf("want 1 disk, got %d", len(m.Disks))
	}
	d := m.Disks[0]
	// /sys/block size is in 512-byte sectors regardless of the device's own
	// sector size — 1953525168 * 512 ≈ 1.0 TB.
	if d.SizeBytes != 1953525168*512 {
		t.Fatalf("disk size = %d", d.SizeBytes)
	}
	if d.Rotational {
		t.Fatal("an NVMe must not be reported as rotational")
	}
	if d.ReadBps != (5096-1000)*512 || d.WriteBps != (30000-2000)*512 {
		t.Fatalf("disk rates = %d/%d B/s", d.ReadBps, d.WriteBps)
	}

	if len(m.Nets) != 1 || m.Nets[0].SpeedMbps != 1200 || m.Nets[0].State != "up" {
		t.Fatalf("nets = %+v", m.Nets)
	}
	if m.Nets[0].RxBps != 1200000 || m.Nets[0].TxBps != 380000 {
		t.Fatalf("net rates = %d/%d B/s", m.Nets[0].RxBps, m.Nets[0].TxBps)
	}

	if len(m.Sensors) != 2 {
		t.Fatalf("want 2 sensors, got %+v", m.Sensors)
	}
	if m.Sensors[1].Celsius != 48 {
		t.Fatalf("x86_pkg_temp = %v °C, want 48", m.Sensors[1].Celsius)
	}

	if m.Platform.OSName != "Ubuntu 24.04.1 LTS" || m.Platform.UptimeSeconds != 1234567 {
		t.Fatalf("platform = %+v", m.Platform)
	}
	if m.Platform.BatteryPct != 100 || m.Platform.OnAC == nil || !*m.Platform.OnAC {
		t.Fatalf("battery/AC = %d %v", m.Platform.BatteryPct, m.Platform.OnAC)
	}
}

// A VM reports almost nothing. It must still parse, be flagged, and explain
// itself rather than looking like a broken probe.
func TestParseMachineProbeVirtualMachine(t *testing.T) {
	probe := "id.vendor\tQEMU\n" +
		"id.product\tStandard PC (i440FX + PIIX, 1996)\n" +
		"cpu.model\tCommon KVM processor\n" +
		"cpu.threads\t4\n" +
		"cpu.cores\t0\n" +
		"cpu.virtual\t1\n" +
		"cpu.load\t0.10 0.20 0.30\n" +
		"mem.MemTotal\t4194304\n" +
		"mem.MemAvailable\t3000000\n"

	m := ParseMachineProbe(probe)
	if !m.Identity.Virtualized || !m.CPU.Virtual {
		t.Fatal("a QEMU host must be flagged as virtual")
	}
	// A kernel that omits `core id` reports 0 cores; falling back to the thread
	// count beats claiming a zero-core machine.
	if m.CPU.Cores != 4 {
		t.Fatalf("cores = %d, want the thread count 4 as fallback", m.CPU.Cores)
	}
	if len(m.GPUs) != 0 || len(m.Disks) != 0 || len(m.Sensors) != 0 {
		t.Fatal("absent subsystems must be empty, not fabricated")
	}
	joined := strings.Join(m.Warnings, " ")
	if !strings.Contains(joined, "virtual machine") {
		t.Fatalf("the result must explain it is a VM, got %v", m.Warnings)
	}
	// Absent clocks must stay zero rather than becoming a bogus reading.
	if m.CPU.MHzNow != 0 || m.CPU.MHzMax != 0 {
		t.Fatalf("absent cpufreq must not invent clocks: %v/%v", m.CPU.MHzNow, m.CPU.MHzMax)
	}
}

// Firmware placeholders are worse than nothing — "To Be Filled By O.E.M." as a
// model name looks like real data.
func TestParseMachineProbeStripsFirmwarePlaceholders(t *testing.T) {
	probe := "id.vendor\tTo Be Filled By O.E.M.\n" +
		"id.product\tSystem Product Name\n" +
		"id.board\tDefault string\n" +
		"disk\tsda|1000|1\n" +
		"disk.model\tsda|To be filled by O.E.M.\n"

	m := ParseMachineProbe(probe)
	if m.Identity.Vendor != "" || m.Identity.Product != "" || m.Identity.Board != "" {
		t.Fatalf("placeholders must be dropped: %+v", m.Identity)
	}
	if m.Disks[0].Model != "" {
		t.Fatalf("placeholder disk model must be dropped, got %q", m.Disks[0].Model)
	}
	if !m.Partial {
		t.Fatal("a host with no usable identity must be marked partial")
	}
}

// A counter reset (reboot, driver reload) must never produce a negative rate.
func TestParseMachineProbeCounterResetIsNotNegative(t *testing.T) {
	probe := "disk\tsda|1000|1\n" +
		"net\teth0|1000|up\n" +
		"a.disk\tsda\t9000\t9000\n" +
		"b.disk\tsda\t10\t10\n" +
		"a.net\teth0\t9000\t9000\n" +
		"b.net\teth0\t10\t10\n"

	m := ParseMachineProbe(probe)
	if m.Disks[0].ReadBps < 0 || m.Disks[0].WriteBps < 0 {
		t.Fatalf("a reset counter must not yield a negative disk rate: %+v", m.Disks[0])
	}
	if m.Nets[0].RxBps < 0 || m.Nets[0].TxBps < 0 {
		t.Fatalf("a reset counter must not yield a negative net rate: %+v", m.Nets[0])
	}
}

// Some firmware parks unused thermal zones at 0 or at a sentinel. Rendering
// those as real temperatures would be worse than omitting them.
func TestParseMachineProbeRejectsImplausibleTemperatures(t *testing.T) {
	probe := "therm\tzone_off|0\n" +
		"therm\tzone_sentinel|2147483647\n" +
		"therm\tzone_real|52000\n"

	m := ParseMachineProbe(probe)
	if len(m.Sensors) != 1 || m.Sensors[0].Label != "zone_real" || m.Sensors[0].Celsius != 52 {
		t.Fatalf("only the plausible sensor must survive: %+v", m.Sensors)
	}
}

// A link that is down reports speed -1, which is "unknown", not a speed.
func TestParseMachineProbeDownLinkHasNoSpeed(t *testing.T) {
	m := ParseMachineProbe("net\teth1|-1|down\n")
	if len(m.Nets) != 1 {
		t.Fatalf("nets = %+v", m.Nets)
	}
	if m.Nets[0].SpeedMbps != 0 {
		t.Fatalf("a down link must report no speed, got %d", m.Nets[0].SpeedMbps)
	}
	if m.Nets[0].State != "down" {
		t.Fatalf("state = %q", m.Nets[0].State)
	}
}

// Garbage in must not panic or fabricate — the probe runs against hosts we have
// never seen.
func TestParseMachineProbeGarbage(t *testing.T) {
	for _, in := range []string{"", "\n\n\n", "no-tabs-here", "gpu\n", "disk\t\n", "\t\t\t"} {
		m := ParseMachineProbe(in)
		if m == nil {
			t.Fatalf("must never return nil for %q", in)
		}
		if m.ProbedAt == 0 {
			t.Fatal("a result must always be timestamped")
		}
	}
}

// A nil slice marshals to JSON `null`, and a browser doing `warnings.length` on
// null throws. This crashed every host that produced NO warnings at all — DMI
// present, a GPU present, sensors present, not virtual — which is exactly the
// well-equipped machine most likely to be looked at.
func TestParseMachineProbeNeverEmitsNullLists(t *testing.T) {
	// A host that triggers no warning at all.
	complete := "id.vendor\tDell Inc.\nid.product\tPowerEdge R740\n" +
		"gpu\tcard0|0x102b|0x0522|mgag200|\n" +
		"therm\tcpu|45000\n"

	for name, in := range map[string]string{"complete": complete, "empty": ""} {
		m := ParseMachineProbe(in)
		if m.Warnings == nil {
			t.Fatalf("%s: warnings must be an empty list, never nil (JSON null crashes the client)", name)
		}
		if m.GPUs == nil || m.Disks == nil || m.Nets == nil || m.Sensors == nil {
			t.Fatalf("%s: no list may be nil: gpus=%v disks=%v nets=%v sensors=%v",
				name, m.GPUs == nil, m.Disks == nil, m.Nets == nil, m.Sensors == nil)
		}
	}

	// Prove it at the wire level too — the client only ever sees JSON.
	b, err := json.Marshal(ParseMachineProbe(complete))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"warnings":null`, `"gpus":null`, `"disks":null`, `"nets":null`, `"sensors":null`} {
		if strings.Contains(string(b), field) {
			t.Fatalf("serialized payload contains %s — the client will crash on .length", field)
		}
	}
}

// eMMC exposes boot0/boot1/rpmb as separate /sys/block entries belonging to the
// SAME chip. Listing them was showing three phantom 4 MB "disks" beside the real
// one.
func TestParseMachineProbeDropsSubDevices(t *testing.T) {
	probe := "disk\tmmcblk2|60751872|0\n" +
		"disk.model\tmmcblk2|SanDisk DF4032\n" +
		"disk\tmmcblk2boot0|8192|0\n" +
		"disk\tmmcblk2boot1|8192|0\n" +
		"disk\tmmcblk2rpmb|8192|0\n" +
		"disk\tnvme0n1|1953525168|0\n" +
		"disk.model\tnvme0n1|Samsung\n" +
		"disk\tnvme0n1p1|1000|0\n"

	m := ParseMachineProbe(probe)
	got := []string{}
	for _, d := range m.Disks {
		got = append(got, d.Name)
	}
	want := []string{"mmcblk2", "nvme0n1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("disks = %v, want only the whole devices %v", got, want)
	}
}

// Two unrelated devices must not be mistaken for parent/child just because one
// name sorts near the other.
func TestParseMachineProbeKeepsSiblingDisks(t *testing.T) {
	m := ParseMachineProbe("disk\tsda|100|1\n" +
		"disk.model\tsda|A\ndisk\tsdb|100|1\n" +
		"disk.model\tsdb|B\ndisk\tnvme0n1|100|0\n" +
		"disk.model\tnvme0n1|C\ndisk\tnvme0n2|100|0\n" +
		"disk.model\tnvme0n2|D\n")
	if len(m.Disks) != 4 {
		t.Fatalf("sibling devices must all survive, got %d: %+v", len(m.Disks), m.Disks)
	}
}

// The NVIDIA driver publishes the exact marketing name; it must win over any
// database lookup, and must only be applied to NVIDIA cards.
func TestParseMachineProbeGPUNameResolution(t *testing.T) {
	probe := "gpu\tcard0|0x10de|0x2560|nvidia|GA106M\n" +
		"gpu\tcard1|0x8086|0x9a60|i915|TigerLake-H GT1 [UHD Graphics]\n" +
		"gpu.nvidia\tNVIDIA GeForce RTX 3060 Laptop GPU\n"

	m := ParseMachineProbe(probe)
	if len(m.GPUs) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(m.GPUs))
	}
	if m.GPUs[0].Name != "NVIDIA GeForce RTX 3060 Laptop GPU" {
		t.Fatalf("the NVIDIA driver's own name must win, got %q", m.GPUs[0].Name)
	}
	// The Intel card keeps its pci.ids name — the NVIDIA name must not bleed onto it.
	if m.GPUs[1].Name != "TigerLake-H GT1 [UHD Graphics]" {
		t.Fatalf("the Intel card must keep its pci.ids name, got %q", m.GPUs[1].Name)
	}
}

// A host with no pci.ids and no NVIDIA driver must degrade to vendor + id rather
// than showing an empty name.
func TestParseMachineProbeGPUWithoutDatabase(t *testing.T) {
	m := ParseMachineProbe("gpu\tcard0|0x8086|0x22b0|i915|\n")
	if len(m.GPUs) != 1 {
		t.Fatalf("gpus = %+v", m.GPUs)
	}
	if m.GPUs[0].Name != "" {
		t.Fatalf("no name must be reported when the host has no database, got %q", m.GPUs[0].Name)
	}
	if m.GPUs[0].Vendor != "Intel" || m.GPUs[0].DeviceID != "0x22b0" {
		t.Fatalf("vendor/id fallback missing: %+v", m.GPUs[0])
	}
}

// GPU telemetry comes from the driver's own hwmon node, which is how a GPU
// temperature is obtained WITHOUT nvidia-smi or device passthrough.
func TestParseMachineProbeGPUTelemetry(t *testing.T) {
	probe := "gpu\tcard0|0x10de|0x2560|nvidia|\n" +
		"gpu.nvidia\tNVIDIA GeForce RTX 3060 Laptop GPU\n" +
		"gpu.sensor\tcard0|temp|64000\n" +
		"gpu.sensor\tcard0|fan|1800\n" +
		"gpu.sensor\tcard0|power|45000000\n"

	m := ParseMachineProbe(probe)
	if len(m.GPUs) != 1 {
		t.Fatalf("gpus = %+v", m.GPUs)
	}
	g := m.GPUs[0]
	if g.Name != "NVIDIA GeForce RTX 3060 Laptop GPU" {
		t.Fatalf("name = %q", g.Name)
	}
	if g.TempC != 64 {
		t.Fatalf("temp = %v °C, want 64 (milli-degrees)", g.TempC)
	}
	if g.FanRPM != 1800 {
		t.Fatalf("fan = %d rpm", g.FanRPM)
	}
	if g.PowerW != 45 {
		t.Fatalf("power = %v W, want 45 (micro-watts)", g.PowerW)
	}
	// The proprietary NVIDIA driver publishes no utilisation or VRAM to sysfs —
	// those must stay absent rather than being reported as 0.
	if g.UsagePct != 0 || g.VRAMTotal != 0 {
		t.Fatalf("nvidia must not fabricate usage/VRAM: %+v", g)
	}
}

// amdgpu publishes real utilisation and VRAM; those must land too.
func TestParseMachineProbeAMDGPUTelemetry(t *testing.T) {
	probe := "gpu\tcard0|0x1002|0x73df|amdgpu|Navi 22 [Radeon RX 6700 XT]\n" +
		"gpu.sensor\tcard0|temp|51000\n" +
		"gpu.sensor\tcard0|busy|37\n" +
		"gpu.sensor\tcard0|vramtotal|12884901888\n" +
		"gpu.sensor\tcard0|vramused|2147483648\n"

	g := ParseMachineProbe(probe).GPUs[0]
	if g.UsagePct != 37 || g.TempC != 51 {
		t.Fatalf("usage/temp = %v/%v", g.UsagePct, g.TempC)
	}
	if g.VRAMTotal != 12884901888 || g.VRAMUsed != 2147483648 {
		t.Fatalf("vram = %d/%d", g.VRAMUsed, g.VRAMTotal)
	}
}

// Blank and implausible sensor values must be DROPPED, not stored as zero:
// "no fan sensor" and "fan stopped" are different facts.
func TestParseMachineProbeGPUSensorsRejectJunk(t *testing.T) {
	probe := "gpu\tcard0|0x8086|0x22b0|i915|\n" +
		"gpu.sensor\tcard0|temp|\n" +
		"gpu.sensor\tcard0|fan|0\n" +
		"gpu.sensor\tcard0|power|0\n" +
		"gpu.sensor\tcard0|busy|\n"

	g := ParseMachineProbe(probe).GPUs[0]
	if g.TempC != 0 || g.FanRPM != 0 || g.PowerW != 0 || g.UsagePct != 0 {
		t.Fatalf("junk sensor values must be dropped: %+v", g)
	}
	// A sensor line for a card we don't know about must not create one.
	if len(ParseMachineProbe("gpu.sensor\tcard9|temp|50000\n").GPUs) != 0 {
		t.Fatal("a stray sensor line must not invent a GPU")
	}
}

// hwmon is where the real temperatures live. Reading only thermal_zone was why a
// machine could show two vague ACPI zones and no CPU temperature at all.
func TestParseMachineProbeHwmonSensors(t *testing.T) {
	probe := "hwmon\tcoretemp|Package id 0|57000\n" +
		"hwmon\tcoretemp|Core 0|55000\n" +
		"hwmon\tnvme|Composite|41000\n" +
		"hwmon\tnvidia||64000\n" +
		"hwmon\tbroken|Sentinel|2147483647\n" +
		"therm\tacpitz|45500\n"

	m := ParseMachineProbe(probe)
	got := map[string]float64{}
	for _, s := range m.Sensors {
		got[s.Label] = s.Celsius
	}
	if got["coretemp · Package id 0"] != 57 {
		t.Fatalf("labelled hwmon sensor missing: %+v", m.Sensors)
	}
	if got["nvme · Composite"] != 41 {
		t.Fatalf("nvme sensor missing: %+v", m.Sensors)
	}
	// An unlabelled hwmon entry falls back to the chip name alone.
	if got["nvidia"] != 64 {
		t.Fatalf("unlabelled hwmon sensor must use the chip name: %+v", m.Sensors)
	}
	// thermal_zone still contributes.
	if got["acpitz"] != 45.5 {
		t.Fatalf("thermal_zone must still be merged: %+v", m.Sensors)
	}
	if _, bad := got["broken · Sentinel"]; bad {
		t.Fatal("an implausible hwmon reading must be dropped")
	}
}

// Two IDENTICAL cards in one machine are two GPUs. Deduping on the PCI id (as
// this once did) silently merged them, and their sensors cross-contaminated.
func TestParseMachineProbeIdenticalDualGPU(t *testing.T) {
	probe := "gpu\tcard0|0x1002|0x73df|amdgpu|Navi 22\n" +
		"gpu\tcard1|0x1002|0x73df|amdgpu|Navi 22\n" +
		"gpu.sensor\tcard0|temp|51000\n" +
		"gpu.sensor\tcard1|temp|72000\n" +
		"gpu.sensor\tcard0|busy|10\n" +
		"gpu.sensor\tcard1|busy|95\n"

	m := ParseMachineProbe(probe)
	if len(m.GPUs) != 2 {
		t.Fatalf("two identical cards must stay two GPUs, got %d", len(m.GPUs))
	}
	byCard := map[string]MachineGPU{}
	for _, g := range m.GPUs {
		byCard[g.Card] = g
	}
	if byCard["card0"].TempC != 51 || byCard["card1"].TempC != 72 {
		t.Fatalf("sensors landed on the wrong card: %+v", m.GPUs)
	}
	if byCard["card0"].UsagePct != 10 || byCard["card1"].UsagePct != 95 {
		t.Fatalf("utilisation crossed between cards: %+v", m.GPUs)
	}
}

// AMD is the best-served vendor: temperature AND utilisation AND VRAM, all from
// sysfs with no vendor tooling. This pins the full set.
func TestParseMachineProbeAMDFullTelemetry(t *testing.T) {
	probe := "gpu\tcard0|0x1002|0x744c|amdgpu|Navi 31 [Radeon RX 7900 XTX]\n" +
		"gpu.sensor\tcard0|temp|48000\n" +
		"gpu.sensor\tcard0|fan|1150\n" +
		// RDNA3 reports power1_input, not power1_average — the probe falls back,
		// so by the time it reaches the parser it is just "power".
		"gpu.sensor\tcard0|power|32000000\n" +
		"gpu.sensor\tcard0|busy|64\n" +
		"gpu.sensor\tcard0|vramtotal|25757220864\n" +
		"gpu.sensor\tcard0|vramused|3221225472\n" +
		"hwmon\tamdgpu|junction|61000\n" +
		"hwmon\tamdgpu|mem|58000\n"

	m := ParseMachineProbe(probe)
	g := m.GPUs[0]
	if g.Name != "Navi 31 [Radeon RX 7900 XTX]" || g.Vendor != "AMD" {
		t.Fatalf("identity: %+v", g)
	}
	if g.TempC != 48 || g.FanRPM != 1150 || g.PowerW != 32 {
		t.Fatalf("temp/fan/power = %v/%v/%v", g.TempC, g.FanRPM, g.PowerW)
	}
	if g.UsagePct != 64 {
		t.Fatalf("amdgpu utilisation = %v, want 64", g.UsagePct)
	}
	if g.VRAMTotal != 25757220864 || g.VRAMUsed != 3221225472 {
		t.Fatalf("vram = %d/%d", g.VRAMUsed, g.VRAMTotal)
	}
	// AMD's junction/hotspot and memory sensors reach the Temperature tile via
	// the hwmon sweep, labelled, rather than being lost.
	labels := map[string]float64{}
	for _, s := range m.Sensors {
		labels[s.Label] = s.Celsius
	}
	if labels["amdgpu · junction"] != 61 || labels["amdgpu · mem"] != 58 {
		t.Fatalf("amdgpu junction/mem sensors missing: %+v", m.Sensors)
	}
}

// A NAS with disks in standby is the case that produced "machine probe timed
// out" and a blank page. Waking a spun-down drive blocks in UNINTERRUPTIBLE I/O,
// which no timeout can cut short — so the probe has to survive being killed
// mid-run and still report what it managed to read.
//
// The script writes cheapest-first and puts the block-device section LAST, so a
// truncated capture looks exactly like this.
func TestParseMachineProbeSurvivesTruncatedCapture(t *testing.T) {
	// Everything up to (and including) the network section arrived; the probe was
	// killed before the disk loop, so there is no probe.done marker.
	truncated := "id.vendor\tSynology\n" +
		"id.product\tDS920+\n" +
		"cpu.model\tIntel(R) Celeron(R) J4125 CPU @ 2.00GHz\n" +
		"cpu.threads\t4\n" +
		"cpu.cores\t4\n" +
		"cpu.load\t0.31 0.28 0.25\n" +
		"mem.MemTotal\t8388608\n" +
		"mem.MemAvailable\t6000000\n" +
		"os.name\tSynology DSM\n" +
		"net\teth0|1000|up\n"

	m := ParseMachineProbe(truncated)
	if m.Complete {
		t.Fatal("a capture with no completion marker must not report Complete")
	}
	// The whole point: the sections that DID answer are still usable.
	if m.Identity.Product != "DS920+" || m.CPU.Cores != 4 {
		t.Fatalf("identity/CPU must survive a truncated capture: %+v %+v", m.Identity, m.CPU)
	}
	if m.Memory.TotalBytes == 0 {
		t.Fatal("memory must survive a truncated capture")
	}
	if len(m.Nets) != 1 {
		t.Fatalf("network must survive a truncated capture: %+v", m.Nets)
	}
	// And the part that hung is simply absent, not fabricated.
	if len(m.Disks) != 0 {
		t.Fatalf("no disks were reported, so none must be invented: %+v", m.Disks)
	}
}

// A complete capture sets the marker, so "incomplete" is never claimed of a
// probe that actually finished.
func TestParseMachineProbeCompleteMarker(t *testing.T) {
	if !ParseMachineProbe("cpu.threads\t4\nprobe.done\t1\n").Complete {
		t.Fatal("probe.done must mark the capture complete")
	}
	if ParseMachineProbe("cpu.threads\t4\n").Complete {
		t.Fatal("a capture with no marker must not be reported complete")
	}
}

// A disk whose model read hung is still a disk. The model is emitted on its own
// line precisely so losing it doesn't lose the drive.
func TestParseMachineProbeDiskWithoutModel(t *testing.T) {
	m := ParseMachineProbe("disk\tsda|1953525168|1\ndisk\tsdb|1953525168|1\ndisk.model\tsda|WDC WD40EFRX\n")
	if len(m.Disks) != 2 {
		t.Fatalf("both disks must be reported, got %d", len(m.Disks))
	}
	byName := map[string]MachineDisk{}
	for _, d := range m.Disks {
		byName[d.Name] = d
	}
	if byName["sda"].Model != "WDC WD40EFRX" {
		t.Fatalf("sda model = %q", byName["sda"].Model)
	}
	if byName["sdb"].Model != "" {
		t.Fatalf("a missing model must be empty, not invented: %q", byName["sdb"].Model)
	}
	if byName["sdb"].SizeBytes == 0 || !byName["sdb"].Rotational {
		t.Fatalf("cheap attributes must still land without a model: %+v", byName["sdb"])
	}
}

// Rates are per-second. A slow host takes far longer than the nominal window
// between the two samples, and dividing by the nominal value would report a
// throughput several times higher than reality.
func TestParseMachineProbeRatesUseTheRealWindow(t *testing.T) {
	// 5 real seconds elapsed, not the nominal 1.
	probe := "disk\tsda|1000|1\n" +
		"net\teth0|1000|up\n" +
		"a.at\t1000\n" +
		"a.disk\tsda\t0\t0\n" +
		"a.net\teth0\t0\t0\n" +
		"b.at\t1005\n" +
		"b.disk\tsda\t10240\t0\n" +
		"b.net\teth0\t5000000\t0\n"

	m := ParseMachineProbe(probe)
	// 10240 sectors * 512 B over 5 s = 1 048 576 B/s.
	if got := m.Disks[0].ReadBps; got != 1048576 {
		t.Fatalf("disk read = %d B/s, want 1048576 (rate must use the REAL 5s window)", got)
	}
	if got := m.Nets[0].RxBps; got != 1000000 {
		t.Fatalf("net rx = %d B/s, want 1000000", got)
	}

	// Without timestamps (an older probe) it falls back to the nominal window.
	legacy := "disk\tsda|1000|1\na.disk\tsda\t0\t0\nb.disk\tsda\t10240\t0\n"
	if got := ParseMachineProbe(legacy).Disks[0].ReadBps; got != 10240*512 {
		t.Fatalf("legacy fallback = %d B/s, want %d", got, 10240*512)
	}
}

// Free space is a FILESYSTEM property, so it arrives separately from the disk
// and has to be mapped back. Getting that mapping wrong is how a 16 TB array
// ends up reporting a 512 MB boot partition's free space.
func TestParseMachineProbeFilesystemUsage(t *testing.T) {
	probe := "disk\tsda|3907029168|1\n" +
		"disk.model\tsda|WDC WD20EFZX\n" +
		"disk\tnvme0n1|1000215216|0\n" +
		"disk.model\tnvme0n1|Samsung SSD 980\n" +
		// df -P -k: device|mount|1K-blocks|used|available
		"fs\t/dev/sda1|/mnt/data|1922728448|1200000000|624000000\n" +
		"fs\t/dev/sda2|/mnt/scratch|30000000|1000000|27000000\n" +
		"fs\t/dev/nvme0n1p2|/|480000000|120000000|340000000\n" +
		"fs\t/dev/nvme0n1p1|/boot/efi|523248|6000|517248\n" +
		// A filesystem on something not listed as a disk (LVM/mdraid) must be
		// reported in the full list but attributed to no disk.
		"fs\t/dev/mapper/vg-root|/srv|100000|50000|50000\n"

	m := ParseMachineProbe(probe)
	if len(m.Filesystems) != 5 {
		t.Fatalf("want 5 filesystems, got %d", len(m.Filesystems))
	}

	byName := map[string]MachineDisk{}
	for _, d := range m.Disks {
		byName[d.Name] = d
	}

	// sda carries two filesystems; the disk's totals are their sum.
	sda := byName["sda"]
	if len(sda.Filesystems) != 2 {
		t.Fatalf("sda should carry 2 filesystems, got %d", len(sda.Filesystems))
	}
	const k = 1024
	if sda.FSFreeBytes != (624000000+27000000)*k {
		t.Fatalf("sda free = %d, want the SUM of its filesystems", sda.FSFreeBytes)
	}
	if sda.FSTotalBytes != (1922728448+30000000)*k {
		t.Fatalf("sda total = %d", sda.FSTotalBytes)
	}

	// nvme0n1p2 must land on nvme0n1 — a trailing-digit strip would look for a
	// non-existent "nvme0n" and lose it entirely.
	nvme := byName["nvme0n1"]
	if len(nvme.Filesystems) != 2 {
		t.Fatalf("nvme0n1 should carry 2 filesystems, got %d: %+v", len(nvme.Filesystems), nvme.Filesystems)
	}
	if nvme.FSFreeBytes != (340000000+517248)*k {
		t.Fatalf("nvme free = %d", nvme.FSFreeBytes)
	}

	// The LVM volume is listed but attributed to no disk.
	attributed := 0
	for _, d := range m.Disks {
		attributed += len(d.Filesystems)
	}
	if attributed != 4 {
		t.Fatalf("exactly 4 filesystems map to a listed disk, got %d", attributed)
	}
}

// A disk with nothing mounted is a real answer (a spare bay, an unformatted
// drive), not missing data.
func TestParseMachineProbeDiskWithNoFilesystem(t *testing.T) {
	m := ParseMachineProbe("disk\tsdc|3907029168|1\ndisk.model\tsdc|Spare\n")
	if len(m.Disks) != 1 {
		t.Fatalf("disks = %+v", m.Disks)
	}
	d := m.Disks[0]
	if len(d.Filesystems) != 0 || d.FSTotalBytes != 0 || d.FSFreeBytes != 0 {
		t.Fatalf("an unmounted disk must report no filesystem totals: %+v", d)
	}
	if d.SizeBytes == 0 {
		t.Fatal("its capacity must still be reported")
	}
}

// A host that refused the /hostfs mount reports no filesystems at all, and the
// rest of the probe must be unaffected.
func TestParseMachineProbeWithoutFilesystemMount(t *testing.T) {
	m := ParseMachineProbe("disk\tsda|3907029168|1\ncpu.threads\t8\n")
	if m.Filesystems == nil {
		t.Fatal("filesystems must be an empty list, never nil (JSON null crashes the client)")
	}
	if len(m.Filesystems) != 0 || m.Disks[0].FSTotalBytes != 0 {
		t.Fatalf("no df output must mean no usage claimed: %+v", m.Disks[0])
	}
	if m.CPU.Threads != 8 {
		t.Fatal("the rest of the probe must be unaffected")
	}
}

// df reports 1K blocks; a unit slip here would misreport every size by 1024x.
func TestParseFilesystemsConvertsBlocksToBytes(t *testing.T) {
	fs := parseFilesystems([]string{"/dev/sda1|/|1000|400|600"})
	if len(fs) != 1 {
		t.Fatalf("fs = %+v", fs)
	}
	if fs[0].TotalBytes != 1000*1024 || fs[0].UsedBytes != 400*1024 || fs[0].FreeBytes != 600*1024 {
		t.Fatalf("df 1K blocks must convert to bytes: %+v", fs[0])
	}
	// A bind mount reports the same filesystem twice; it must be counted once.
	dup := parseFilesystems([]string{"/dev/sda1|/|1000|400|600", "/dev/sda1|/|1000|400|600"})
	if len(dup) != 1 {
		t.Fatalf("duplicate device+mount must collapse, got %d", len(dup))
	}
	// A zero-size pseudo filesystem tells us nothing.
	if len(parseFilesystems([]string{"/dev/loop0|/snap|0|0|0"})) != 0 {
		t.Fatal("a zero-total filesystem must be dropped")
	}
}
