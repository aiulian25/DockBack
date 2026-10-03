package dockercli

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Parsing for the host machine probe (F105). Kept PURE and separate from the
// container plumbing so every shape of real-world output — a laptop with
// thermal zones, a VM with almost nothing, a NAS that reports no DMI at all —
// is unit-testable without a Docker daemon.
//
// The governing rule: absence is never an error and never a zero. A field the
// host didn't report is simply left unset so the UI can say "not reported"
// rather than confidently displaying 0 °C or a 0-byte disk.

// chassisTypes maps the SMBIOS chassis-type number to a word. Only the values
// worth distinguishing are named; anything else falls through to "".
var chassisTypes = map[string]string{
	"1": "Other", "2": "Unknown", "3": "Desktop", "4": "Low-profile desktop",
	"5": "Pizza box", "6": "Mini tower", "7": "Tower", "8": "Portable",
	"9": "Laptop", "10": "Notebook", "11": "Handheld", "13": "All-in-one",
	"14": "Sub-notebook", "15": "Space-saving", "16": "Lunch box",
	"17": "Main server chassis", "18": "Expansion chassis", "21": "Peripheral",
	"22": "RAID chassis", "23": "Rack mount", "24": "Sealed-case PC",
	"28": "Blade", "30": "Tablet", "31": "Convertible", "32": "Detachable",
}

// pciVendors names the handful of GPU vendors worth resolving. Full DEVICE
// names would need the ~1 MB hwdata PCI database, which is not worth shipping in
// a backup tool — so the UI shows vendor + driver + the raw id and says so,
// rather than inventing a model name it cannot actually know.
var pciVendors = map[string]string{
	"0x10de": "NVIDIA", "0x1002": "AMD", "0x1022": "AMD",
	"0x8086": "Intel", "0x1a03": "ASPEED", "0x102b": "Matrox",
	"0x15ad": "VMware", "0x1234": "QEMU", "0x1af4": "Red Hat (virtio)",
}

// virtualVendors are DMI vendor strings that mean "this is not real hardware".
var virtualVendors = []string{
	"qemu", "kvm", "vmware", "innotek", "virtualbox", "xen", "bochs",
	"microsoft corporation", "parallels", "google", "amazon ec2", "alibaba",
}

type kvLine struct{ key, val string }

// ParseMachineProbe turns the probe's TAB-separated output into a MachineInfo.
// It never returns an error: a probe that produced only two usable lines still
// yields a valid (Partial) result, which is strictly more useful than failing.
func ParseMachineProbe(out string) *MachineInfo {
	// Every list is seeded NON-NIL. A nil Go slice marshals to JSON `null`, and a
	// client doing `warnings.length` on null throws — which is exactly what
	// happened to any host that produced no warnings at all (DMI present, a GPU
	// present, sensors present, not virtual). Absence must be an empty list.
	m := &MachineInfo{
		ProbedAt: time.Now().Unix(),
		GPUs:     []MachineGPU{}, Disks: []MachineDisk{}, Nets: []MachineNet{},
		Sensors: []MachineSensor{}, Warnings: []string{}, Filesystems: []MachineFS{},
	}
	kv := map[string]string{}
	var gpuLines, diskLines, netLines, thermLines, nvidiaNames []string
	var gpuSensorLines, hwmonLines, diskModels, fsLines []string
	sampleA := map[string][]string{}
	sampleB := map[string][]string{}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		key, rest := parts[0], parts[1:]
		switch {
		case key == "gpu":
			gpuLines = append(gpuLines, rest[0])
		case key == "gpu.sensor":
			gpuSensorLines = append(gpuSensorLines, rest[0])
		case key == "dev":
			m.Devices = append(m.Devices, strings.TrimSpace(rest[0]))
		case key == "hwmon":
			hwmonLines = append(hwmonLines, rest[0])
		case key == "gpu.nvidia":
			nvidiaNames = append(nvidiaNames, strings.TrimSpace(rest[0]))
		case key == "disk":
			diskLines = append(diskLines, rest[0])
		case key == "fs":
			fsLines = append(fsLines, rest[0])
		case key == "disk.model":
			diskModels = append(diskModels, rest[0])
		case key == "probe.done":
			m.Complete = true
		case key == "net":
			netLines = append(netLines, rest[0])
		case key == "therm":
			thermLines = append(thermLines, rest[0])
		case strings.HasPrefix(key, "a."):
			sampleA[key[2:]] = append(sampleA[key[2:]], strings.Join(rest, "\t"))
		case strings.HasPrefix(key, "b."):
			sampleB[key[2:]] = append(sampleB[key[2:]], strings.Join(rest, "\t"))
		default:
			kv[key] = strings.TrimSpace(rest[0])
		}
	}

	parseIdentity(m, kv)
	parseCPU(m, kv, sampleA, sampleB)
	parseMemory(m, kv)
	m.GPUs = parseGPUs(gpuLines, nvidiaNames, gpuSensorLines)
	attachHwmonToGPUs(m.GPUs, hwmonLines)
	m.Filesystems = parseFilesystems(fsLines)
	m.Disks = parseDisks(diskLines, diskModels, sampleA["disk"], sampleB["disk"], elapsed(sampleA, sampleB))
	attachFilesystems(m.Disks, m.Filesystems)
	m.Nets = parseNets(netLines, sampleA["net"], sampleB["net"], elapsed(sampleA, sampleB))
	m.Sensors = parseSensors(thermLines, hwmonLines)
	parsePlatform(m, kv)
	sort.Strings(m.Devices)

	// Say what wasn't there, so the page can explain a sparse result instead of
	// leaving the operator wondering whether the probe half-failed.
	if m.Identity.Vendor == "" && m.Identity.Product == "" {
		m.Warnings = append(m.Warnings, "This host reports no DMI identity — common on a VM, a container-only host, or a NAS with a locked-down firmware.")
		m.Partial = true
	}
	if len(m.GPUs) == 0 {
		m.Warnings = append(m.Warnings, "No graphics device is exposed to the host kernel.")
	}
	if len(m.Sensors) == 0 {
		m.Warnings = append(m.Warnings, "This host exposes no temperature sensors.")
	}
	if m.Identity.Virtualized || m.CPU.Virtual {
		m.Warnings = append(m.Warnings, "This is a virtual machine — the hardware below is what the hypervisor presents, not the physical host.")
	}
	return m
}

func parseIdentity(m *MachineInfo, kv map[string]string) {
	id := &m.Identity
	id.Vendor = clean(kv["id.vendor"])
	id.Product = clean(kv["id.product"])
	id.Version = clean(kv["id.version"])
	id.BoardVendor = clean(kv["id.board_vendor"])
	id.Board = clean(kv["id.board"])
	id.BIOSVersion = clean(kv["id.bios_version"])
	id.BIOSDate = clean(kv["id.bios_date"])
	id.ChassisType = chassisTypes[strings.TrimSpace(kv["id.chassis"])]

	low := strings.ToLower(id.Vendor + " " + id.Product)
	for _, v := range virtualVendors {
		if strings.Contains(low, v) {
			id.Virtualized = true
			break
		}
	}
}

// clean drops the placeholder strings firmware vendors ship when a field was
// never populated. Showing "To Be Filled By O.E.M." as a model name is worse
// than showing nothing.
func clean(s string) string {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	for _, junk := range []string{
		"to be filled by o.e.m.", "to be filled by oem", "default string",
		"system product name", "system manufacturer", "not specified",
		"not applicable", "none", "n/a", "unknown", "0123456789",
	} {
		if low == junk {
			return ""
		}
	}
	return s
}

func parseCPU(m *MachineInfo, kv map[string]string, a, b map[string][]string) {
	c := &m.CPU
	c.Model = clean(kv["cpu.model"])
	c.Threads = atoi(kv["cpu.threads"])
	c.Cores = atoi(kv["cpu.cores"])
	// A single-socket read of `core id` undercounts nothing, but a kernel that
	// omits it entirely reports 0 — fall back to threads so the UI never claims
	// a zero-core machine.
	if c.Cores == 0 {
		c.Cores = c.Threads
	}
	c.Virtual = kv["cpu.virtual"] == "1"
	if cache := kv["cpu.cache"]; cache != "" {
		c.CacheKB = atoi(strings.Fields(cache)[0])
	}
	// cpufreq reports kHz.
	c.MHzMin = float64(atoi(kv["cpu.mhz_min"])) / 1000
	c.MHzMax = float64(atoi(kv["cpu.mhz_max"])) / 1000
	c.MHzNow = float64(atoi(kv["cpu.mhz_cur"])) / 1000

	if f := strings.Fields(kv["cpu.load"]); len(f) >= 3 {
		c.Load1, _ = strconv.ParseFloat(f[0], 64)
		c.Load5, _ = strconv.ParseFloat(f[1], 64)
		c.Load15, _ = strconv.ParseFloat(f[2], 64)
	}

	// CPU% from the two /proc/stat samples: busy delta over total delta. This is
	// TRUE host utilisation — unlike the dashboard's figure, which sums container
	// stats and so misses everything running outside Docker.
	ba, ia, okA := statSample(a["cpu"])
	bb, ib, okB := statSample(b["cpu"])
	if okA && okB {
		dBusy, dIdle := bb-ba, ib-ia
		if total := dBusy + dIdle; total > 0 {
			c.UsagePct = round1(float64(dBusy) / float64(total) * 100)
		}
	}
}

// statSample pulls (busy, idle) out of one sampled `/proc/stat` cpu line.
func statSample(lines []string) (busy, idle int64, ok bool) {
	if len(lines) == 0 {
		return 0, 0, false
	}
	f := strings.Split(lines[0], "\t")
	if len(f) < 2 {
		return 0, 0, false
	}
	return atoi64(f[0]), atoi64(f[1]), true
}

func parseMemory(m *MachineInfo, kv map[string]string) {
	const k = 1024
	mem := &m.Memory
	mem.TotalBytes = int64(atoi(kv["mem.MemTotal"])) * k
	mem.AvailableBytes = int64(atoi(kv["mem.MemAvailable"])) * k
	mem.CachedBytes = int64(atoi(kv["mem.Cached"])) * k
	mem.SwapTotalBytes = int64(atoi(kv["mem.SwapTotal"])) * k
	free := int64(atoi(kv["mem.SwapFree"])) * k
	if mem.SwapTotalBytes > 0 {
		mem.SwapUsedBytes = mem.SwapTotalBytes - free
	}
	// "Used" means what applications hold — total minus AVAILABLE, not minus
	// free. Cache is reclaimable, and counting it as used is the classic way to
	// make a healthy Linux box look like it is out of memory.
	if mem.TotalBytes > 0 && mem.AvailableBytes > 0 {
		mem.UsedBytes = mem.TotalBytes - mem.AvailableBytes
	}
}

// parseGPUs resolves each card as precisely as the host allowed.
//
// Name resolution, best source first:
//  1. the NVIDIA proprietary driver's own /proc entry — the exact marketing name
//     ("NVIDIA GeForce RTX 3060 Laptop GPU"), better than any database;
//  2. the HOST's pci.ids, when it ships one, looked up by vendor+device;
//  3. vendor name + raw device id, which is all we can honestly claim.
//
// DockBack never ships a PCI database of its own: it would be a megabyte of
// lookup tables in a backup tool, and it would go stale.
func parseGPUs(lines, nvidiaNames, sensorLines []string) []MachineGPU {
	out := []MachineGPU{}
	seen := map[string]bool{}
	for _, l := range lines {
		f := splitN(l, "|", 6)
		card, vendorID, deviceID, driver, name, pci := f[0], f[1], f[2], f[3], f[4], f[5]
		// Keyed by the DRM card node, not vendor+device: two identical cards in
		// one machine are two GPUs, and deduping on the PCI id would silently
		// merge them into one.
		if card == "" || vendorID == "" || seen[card] {
			continue
		}
		seen[card] = true
		out = append(out, MachineGPU{
			Card: card, Vendor: pciVendors[strings.ToLower(vendorID)],
			VendorID: vendorID, DeviceID: deviceID, Driver: driver, Name: clean(name),
			PCIAddr: pci,
		})
	}
	// Telemetry, keyed by vendor|device so it lands on the right card.
	applyGPUSensors(out, sensorLines)

	// The NVIDIA driver lists its cards in order; match them onto the NVIDIA
	// entries we found, leaving any other vendor's cards alone.
	ni := 0
	for i := range out {
		if strings.EqualFold(out[i].VendorID, "0x10de") && ni < len(nvidiaNames) {
			if n := clean(nvidiaNames[ni]); n != "" {
				out[i].Name = n
			}
			ni++
		}
	}
	return out
}

// applyGPUSensors folds hwmon telemetry onto the card it belongs to.
//
// Values arrive in the kernel's units: milli-degrees, micro-watts, bytes. A
// blank or implausible reading is DROPPED rather than stored as zero — "no fan
// sensor" and "fan stopped" are different facts, and the UI must be able to tell
// them apart.
func applyGPUSensors(gpus []MachineGPU, lines []string) {
	for _, l := range lines {
		f := splitN(l, "|", 3)
		card, kind, raw := f[0], f[1], f[2]
		if card == "" || kind == "" || raw == "" {
			continue
		}
		for i := range gpus {
			if gpus[i].Card != card {
				continue
			}
			gpus[i].Telemetry = "hwmon"
			switch kind {
			case "temp":
				if v := atoi64(raw); v >= 1000 && v <= 150000 {
					gpus[i].TempC = round1(float64(v) / 1000)
				}
			case "fan":
				if v := atoi(raw); v > 0 && v < 30000 {
					gpus[i].FanRPM = v
				}
			case "power":
				if v := atoi64(raw); v > 0 {
					gpus[i].PowerW = round1(float64(v) / 1e6) // micro-watts
				}
			case "busy":
				if v := atoi(raw); v > 0 && v <= 100 {
					gpus[i].UsagePct = float64(v)
				}
			case "vramtotal":
				gpus[i].VRAMTotal = atoi64(raw)
			case "vramused":
				gpus[i].VRAMUsed = atoi64(raw)
			}
		}
	}
}

// attachHwmonToGPUs fills a GPU's temperature from the GLOBAL hwmon sweep when
// the card's own subtree had none.
//
// amdgpu, nouveau and i915 sometimes register their hwmon node outside the DRM
// device's directory, so a temperature that was visible in /sys/class/hwmon was
// reaching the Sensors tile but never the GPU tile it belongs to.
//
// This cannot help the proprietary NVIDIA driver: it registers no hwmon node at
// all — which is why lm-sensors can't read NVIDIA temperatures either, and why
// nvidia-smi (NVML) is the only source. See probeNvidiaSMI.
func attachHwmonToGPUs(gpus []MachineGPU, hwmonLines []string) {
	for i := range gpus {
		if gpus[i].TempC != 0 || gpus[i].Driver == "" {
			continue
		}
		for _, l := range hwmonLines {
			f := splitN(l, "|", 3)
			if !strings.EqualFold(f[0], gpus[i].Driver) {
				continue
			}
			if v := atoi64(f[2]); v >= 1000 && v <= 150000 {
				gpus[i].TempC = round1(float64(v) / 1000)
				break
			}
		}
	}
}

func parseDisks(lines, models, a, b []string, window float64) []MachineDisk {
	rd, wr := diskRates(a, b, window)
	// The model arrives on its own line: reading it can wake a spun-down drive and
	// block, so it is emitted separately from the cheap attributes. A disk whose
	// model never arrived is still a disk.
	byName := map[string]string{}
	for _, l := range models {
		f := splitN(l, "|", 2)
		if f[0] != "" {
			byName[f[0]] = clean(f[1])
		}
	}
	out := []MachineDisk{}
	for _, l := range lines {
		f := splitN(l, "|", 3)
		if f[0] == "" {
			continue
		}
		d := MachineDisk{
			Name:  f[0],
			Model: byName[f[0]],
			// /sys/block/*/size is in 512-byte sectors, always — not the device's
			// own sector size. Using the physical block size here would inflate a
			// 4Kn disk eightfold.
			SizeBytes:  int64(atoi(f[1])) * 512,
			Rotational: f[2] == "1",
		}
		d.ReadBps, d.WriteBps = rd[d.Name], wr[d.Name]
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return dropSubDevices(out)
}

// elapsed returns the real seconds between the two rate samples.
//
// The script sleeps a nominal window, but a slow host can take far longer
// between the two reads — dividing by the nominal value would then report a
// throughput several times higher than reality. Falls back to the nominal
// window when the timestamps are missing (older probes).
// The timestamps arrive as "a.at"/"b.at", which the "a."/"b." prefix routing
// files under the SAMPLE maps rather than the flat key/value map — so they are
// read from there.
func elapsed(a, b map[string][]string) float64 {
	first, second := firstVal(a["at"]), firstVal(b["at"])
	if first > 0 && second > first {
		return float64(second - first)
	}
	return float64(probeSampleSeconds)
}

func firstVal(v []string) int64 {
	if len(v) == 0 {
		return 0
	}
	return atoi64(v[0])
}

// parseFilesystems turns df output into usage records.
//
// df reports 1K blocks; the numbers are converted to bytes here so nothing
// downstream has to remember the unit. Duplicate device+mount pairs (a bind
// mount shows the same filesystem twice) are collapsed.
func parseFilesystems(lines []string) []MachineFS {
	out := []MachineFS{}
	seen := map[string]bool{}
	for _, l := range lines {
		f := splitN(l, "|", 5)
		dev, mp := f[0], f[1]
		if dev == "" || mp == "" {
			continue
		}
		key := dev + "\x00" + mp
		if seen[key] {
			continue
		}
		seen[key] = true
		const k = 1024
		total := atoi64(f[2]) * k
		if total <= 0 {
			continue // a zero-size pseudo filesystem tells us nothing
		}
		out = append(out, MachineFS{
			Device: dev, Mount: mp, TotalBytes: total,
			UsedBytes: atoi64(f[3]) * k, FreeBytes: atoi64(f[4]) * k,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

// attachFilesystems maps each filesystem onto the disk it lives on and totals
// them, so a disk tile can answer "how much room is left".
//
// Matching is by LONGEST DISK-NAME PREFIX of the device, which is the only rule
// that handles every naming scheme at once: sda1 belongs to sda, nvme0n1p2 to
// nvme0n1 (never to a non-existent "nvme0n"), mmcblk0p1 to mmcblk0. Stripping
// trailing digits would corrupt every NVMe name.
func attachFilesystems(disks []MachineDisk, fss []MachineFS) {
	for _, fs := range fss {
		name := strings.TrimPrefix(fs.Device, "/dev/")
		best := -1
		for i := range disks {
			if !strings.HasPrefix(name, disks[i].Name) {
				continue
			}
			if best < 0 || len(disks[i].Name) > len(disks[best].Name) {
				best = i
			}
		}
		if best < 0 {
			continue // a filesystem on something we did not list (LVM, mdraid, network)
		}
		d := &disks[best]
		d.Filesystems = append(d.Filesystems, fs)
		d.FSTotalBytes += fs.TotalBytes
		d.FSUsedBytes += fs.UsedBytes
		d.FSFreeBytes += fs.FreeBytes
	}
}

// dropSubDevices removes entries that are part of another listed device rather
// than a disk in their own right.
//
// eMMC is the case that motivated this: the kernel exposes mmcblk2 alongside
// mmcblk2boot0, mmcblk2boot1 and mmcblk2rpmb — hardware boot/replay-protect
// areas of the SAME chip, a few MB each. Listing them as four "disks" is wrong
// and clutters the page with 4 MB phantom drives.
//
// The rule is structural rather than a name blacklist: a device whose name is
// another device's name plus a suffix is part of that device. That also covers
// partitions (nvme0n1p1 under nvme0n1) should a kernel ever surface them here.
func dropSubDevices(in []MachineDisk) []MachineDisk {
	out := make([]MachineDisk, 0, len(in))
	for _, d := range in {
		child := false
		for _, other := range in {
			if other.Name != d.Name && strings.HasPrefix(d.Name, other.Name) {
				child = true
				break
			}
		}
		if !child {
			out = append(out, d)
		}
	}
	return out
}

// diskRates converts the two /proc/diskstats samples into bytes/sec. Fields are
// (name, sectors-read, sectors-written); sectors are 512 bytes by kernel
// convention regardless of the device's real sector size.
func diskRates(a, b []string, window float64) (read, write map[string]int64) {
	read, write = map[string]int64{}, map[string]int64{}
	first := map[string][2]int64{}
	for _, l := range a {
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			continue
		}
		first[f[0]] = [2]int64{atoi64(f[1]), atoi64(f[2])}
	}
	for _, l := range b {
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			continue
		}
		p, ok := first[f[0]]
		if !ok {
			continue
		}
		// Counters only ever climb; a negative delta means a reset, so report 0
		// rather than a nonsensical negative rate.
		if dr := atoi64(f[1]) - p[0]; dr > 0 {
			read[f[0]] = int64(float64(dr*512) / window)
		}
		if dw := atoi64(f[2]) - p[1]; dw > 0 {
			write[f[0]] = int64(float64(dw*512) / window)
		}
	}
	return read, write
}

func parseNets(lines []string, a, b []string, window float64) []MachineNet {
	rx, tx := netRates(a, b, window)
	out := []MachineNet{}
	for _, l := range lines {
		f := splitN(l, "|", 3)
		if f[0] == "" {
			continue
		}
		n := MachineNet{Name: f[0], State: f[2]}
		// A down or virtual link reports -1; that is "unknown", not a speed.
		if s := atoi(f[1]); s > 0 {
			n.SpeedMbps = s
		}
		n.RxBps, n.TxBps = rx[n.Name], tx[n.Name]
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func netRates(a, b []string, window float64) (rx, tx map[string]int64) {
	rx, tx = map[string]int64{}, map[string]int64{}
	first := map[string][2]int64{}
	for _, l := range a {
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			continue
		}
		first[f[0]] = [2]int64{atoi64(f[1]), atoi64(f[2])}
	}
	for _, l := range b {
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			continue
		}
		p, ok := first[f[0]]
		if !ok {
			continue
		}
		if d := atoi64(f[1]) - p[0]; d > 0 {
			rx[f[0]] = int64(float64(d) / window)
		}
		if d := atoi64(f[2]) - p[1]; d > 0 {
			tx[f[0]] = int64(float64(d) / window)
		}
	}
	return rx, tx
}

// parseSensors merges the two places Linux keeps temperatures.
//
// thermal_zone is the ACPI view — often just a couple of vague zones. hwmon is
// where the real sensors live: coretemp/k10temp for the CPU package and cores,
// nvme for drives, amdgpu/nvidia for the GPU. Reading only thermal_zone is why a
// machine could show two vague ACPI readings and no CPU temperature at all.
func parseSensors(thermLines, hwmonLines []string) []MachineSensor {
	out := []MachineSensor{}
	seen := map[string]bool{}

	add := func(label string, milli int64) {
		// Firmware parks unused sensors at 0 or at a sentinel; presenting either
		// as a real reading is worse than omitting it.
		if label == "" || milli < 1000 || milli > 150000 || seen[label] {
			return
		}
		seen[label] = true
		out = append(out, MachineSensor{Label: label, Celsius: round1(float64(milli) / 1000)})
	}

	// hwmon first: it is the more precise source, so it wins the dedupe.
	for _, l := range hwmonLines {
		f := splitN(l, "|", 3)
		name, label, raw := f[0], f[1], f[2]
		if name == "" {
			continue
		}
		full := name
		if label != "" {
			full = name + " \u00b7 " + label
		}
		add(full, atoi64(raw))
	}
	for _, l := range thermLines {
		f := splitN(l, "|", 2)
		add(f[0], atoi64(f[1]))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func parsePlatform(m *MachineInfo, kv map[string]string) {
	p := &m.Platform
	p.OSName = clean(kv["os.name"])
	p.Kernel = clean(kv["os.kernel"])
	if f := strings.Fields(kv["sys.uptime"]); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			p.UptimeSeconds = int64(v)
		}
	}
	if b, ok := kv["battery"]; ok && b != "" {
		p.BatteryPct = atoi(b)
	}
	if ac, ok := kv["ac"]; ok && ac != "" {
		on := ac == "1"
		p.OnAC = &on
	}
}

// --- small helpers -------------------------------------------------------

func splitN(s, sep string, n int) []string {
	parts := strings.Split(s, sep)
	out := make([]string, n)
	for i := 0; i < n && i < len(parts); i++ {
		out[i] = strings.TrimSpace(parts[i])
	}
	return out
}

func atoi(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func atoi64(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
