package dockercli

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Host machine probe (F105).
//
// The Docker API tells us almost nothing about the metal: /info gives the OS,
// kernel, architecture, core COUNT and total RAM, and stops there. No vendor, no
// model, no CPU model string, no GPU, no disks, no temperatures. And the CPU/
// memory figures the dashboard shows are the SUM OF CONTAINER STATS, not host
// utilisation — a machine can be at 60% while DockBack reports 2%.
//
// So the real numbers come from the host's own /proc and /sys, read by the same
// short-lived Alpine sidecar the backup path already uses, with those two trees
// bind-mounted READ-ONLY. The container is unprivileged, drops into a plain
// shell, writes nothing, and is removed immediately.
//
// Deliberately NOT read: product_serial, board_serial, chassis_serial. They are
// root-only asset-tracking identifiers with no backup value, and reading them
// would persist hardware serials into the database and into any shared report.
//
// Everything is best-effort by construction: each read is guarded, a missing
// file yields an absent field rather than an error, and the caller renders
// "not reported" instead of a zero. A Synology, a VM or Docker Desktop reports
// the VIRTUAL machine — which is correct, and says so by simply having less.

// maxProbeBytes bounds the probe output. The real payload is a few KB; this is a
// hard stop against a pathological /sys rather than a real limit.
const maxProbeBytes = 1 << 20

// probeSampleSeconds is the in-sidecar delta window. CPU%, disk I/O and network
// throughput are RATES, so the script samples twice this far apart and the Go
// side receives a real rate rather than a meaningless counter.
const probeSampleSeconds = 1

// MachineInfo is one host's hardware and its live utilisation.
type MachineInfo struct {
	Identity MachineIdentity `json:"identity"`
	CPU      MachineCPU      `json:"cpu"`
	Memory   MachineMemory   `json:"memory"`
	GPUs     []MachineGPU    `json:"gpus"`
	Disks    []MachineDisk   `json:"disks"`
	Nets     []MachineNet    `json:"nets"`
	// Filesystems is every mounted, block-backed filesystem on the host — the
	// full picture, including any not attributable to a listed disk.
	Filesystems []MachineFS `json:"filesystems"`
	// Devices are host device nodes (/dev/dri/renderD128, /dev/nvidia0, …), so a
	// cross-host restore can say whether the target has the hardware a container
	// asks for before it is created (F94). Shallow and bounded, never a full walk.
	Devices  []string        `json:"devices,omitempty"`
	Sensors  []MachineSensor `json:"sensors"`
	Platform MachinePlatform `json:"platform"`
	ProbedAt int64           `json:"probed_at"`
	// Complete means the probe script ran to its end marker. False = it was cut
	// short (a slow host, a disk waking from standby), so the reading is whatever
	// arrived before the deadline rather than the whole machine.
	Complete bool     `json:"complete"`
	Partial  bool     `json:"partial"`  // some subsystem reported nothing
	Warnings []string `json:"warnings"` // human notes about what wasn't available
}

type MachineIdentity struct {
	Vendor      string `json:"vendor,omitempty"`
	Product     string `json:"product,omitempty"`
	Version     string `json:"version,omitempty"`
	BoardVendor string `json:"board_vendor,omitempty"`
	Board       string `json:"board,omitempty"`
	BIOSVersion string `json:"bios_version,omitempty"`
	BIOSDate    string `json:"bios_date,omitempty"`
	ChassisType string `json:"chassis_type,omitempty"` // resolved to a word
	Virtualized bool   `json:"virtualized,omitempty"`
}

type MachineCPU struct {
	Model    string  `json:"model,omitempty"`
	Cores    int     `json:"cores,omitempty"`
	Threads  int     `json:"threads,omitempty"`
	CacheKB  int     `json:"cache_kb,omitempty"`
	MHzNow   float64 `json:"mhz_now,omitempty"`
	MHzMin   float64 `json:"mhz_min,omitempty"`
	MHzMax   float64 `json:"mhz_max,omitempty"`
	UsagePct float64 `json:"usage_pct"`
	Load1    float64 `json:"load1"`
	Load5    float64 `json:"load5"`
	Load15   float64 `json:"load15"`
	Virtual  bool    `json:"virtual,omitempty"` // hypervisor flag present
}

type MachineMemory struct {
	TotalBytes     int64 `json:"total_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
	UsedBytes      int64 `json:"used_bytes"`
	CachedBytes    int64 `json:"cached_bytes"`
	SwapTotalBytes int64 `json:"swap_total_bytes"`
	SwapUsedBytes  int64 `json:"swap_used_bytes"`
}

type MachineGPU struct {
	// Card is the DRM node (card0, card1). It is the identity key: two identical
	// cards in one machine share a vendor+device id but are distinct GPUs.
	Card string `json:"card,omitempty"`
	// PCIAddr is the bus address (0000:01:00.0), used to match nvidia-smi output
	// onto the right card rather than trusting enumeration order.
	PCIAddr string `json:"pci_addr,omitempty"`
	// Name is the resolved model where the host could tell us — from the NVIDIA
	// driver's own /proc entry, or the host's pci.ids. Empty when neither was
	// available, in which case the UI shows vendor + device id rather than
	// inventing a model it cannot know.
	Name     string `json:"name,omitempty"`
	Vendor   string `json:"vendor"`           // resolved name where known
	VendorID string `json:"vendor_id"`        // e.g. 0x10de
	DeviceID string `json:"device_id"`        // e.g. 0x2560
	Driver   string `json:"driver,omitempty"` // e.g. nvidia, amdgpu, i915

	// Live telemetry, read from the driver's hwmon node. Every field is optional:
	// a driver that doesn't publish one simply has no value, and the UI omits the
	// line rather than showing a zero. Utilisation and VRAM are amdgpu-only —
	// the proprietary NVIDIA driver publishes neither outside nvidia-smi.
	TempC     float64 `json:"temp_c,omitempty"`
	FanRPM    int     `json:"fan_rpm,omitempty"`
	PowerW    float64 `json:"power_w,omitempty"`
	UsagePct  float64 `json:"usage_pct,omitempty"`
	VRAMTotal int64   `json:"vram_total_bytes,omitempty"`
	VRAMUsed  int64   `json:"vram_used_bytes,omitempty"`
	// FanPct is fan speed as a PERCENTAGE (nvidia-smi reports it that way);
	// FanRPM is an absolute rate from hwmon. A card reports one or the other.
	FanPct int `json:"fan_pct,omitempty"`
	// Telemetry names where the live numbers came from ("hwmon", "nvidia-smi"),
	// so the page can explain a card that shows identity only.
	Telemetry string `json:"telemetry,omitempty"`
}

// MachineFS is one mounted filesystem's usage. Reported separately from disks
// because the relationship is not one-to-one: a disk may carry several
// filesystems, or none mounted at all.
type MachineFS struct {
	Device     string `json:"device"`
	Mount      string `json:"mount"`
	TotalBytes int64  `json:"total_bytes"`
	UsedBytes  int64  `json:"used_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
}

type MachineDisk struct {
	Name       string `json:"name"`
	Model      string `json:"model,omitempty"`
	SizeBytes  int64  `json:"size_bytes"`
	Rotational bool   `json:"rotational"`
	ReadBps    int64  `json:"read_bps"`
	WriteBps   int64  `json:"write_bps"`
	// Filesystems mounted from this disk, and their totals. Empty when the disk
	// holds nothing mounted (a spare, a passthrough drive, an unformatted bay) —
	// which is a real answer, not a gap.
	Filesystems  []MachineFS `json:"filesystems,omitempty"`
	FSTotalBytes int64       `json:"fs_total_bytes,omitempty"`
	FSUsedBytes  int64       `json:"fs_used_bytes,omitempty"`
	FSFreeBytes  int64       `json:"fs_free_bytes,omitempty"`
}

type MachineNet struct {
	Name      string `json:"name"`
	SpeedMbps int    `json:"speed_mbps,omitempty"`
	State     string `json:"state,omitempty"`
	RxBps     int64  `json:"rx_bps"`
	TxBps     int64  `json:"tx_bps"`
}

type MachineSensor struct {
	Label   string  `json:"label"`
	Celsius float64 `json:"celsius"`
}

type MachinePlatform struct {
	OSName        string `json:"os_name,omitempty"`
	Kernel        string `json:"kernel,omitempty"`
	Arch          string `json:"arch,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds,omitempty"`
	BatteryPct    int    `json:"battery_pct,omitempty"`
	OnAC          *bool  `json:"on_ac,omitempty"`
}

// probeScript reads the host trees mounted at /hp (proc) and /hs (sys) and emits
// TAB-separated key/value lines. Every read is guarded so a host that lacks a
// file simply omits that line — the parser treats absence as "not reported".
//
// Rates (CPU, disk, network) are sampled twice inside the sidecar so what
// arrives is already a rate; the Go side never has to hold counters between
// calls or reason about wrap-around.
const probeScript = `
set +e
P=/hp; S=/hs
r() { timeout 2 cat "$1" 2>/dev/null | head -c 4096 | tr -d '\r' | head -n 1; }
kv() { v=$(r "$2"); [ -n "$v" ] && printf '%s\t%s\n' "$1" "$v"; }

# --- identity (DMI). Serial numbers are deliberately never read. ---
kv id.vendor       $S/class/dmi/id/sys_vendor
kv id.product      $S/class/dmi/id/product_name
kv id.version      $S/class/dmi/id/product_version
kv id.board_vendor $S/class/dmi/id/board_vendor
kv id.board        $S/class/dmi/id/board_name
kv id.bios_version $S/class/dmi/id/bios_version
kv id.bios_date    $S/class/dmi/id/bios_date
kv id.chassis      $S/class/dmi/id/chassis_type

# --- cpu ---
awk -F': ' '/^model name/{print "cpu.model\t" $2; exit}' $P/cpuinfo 2>/dev/null
awk -F': ' '/^cache size/{print "cpu.cache\t" $2; exit}' $P/cpuinfo 2>/dev/null
printf 'cpu.threads\t%s\n' "$(grep -c '^processor' $P/cpuinfo 2>/dev/null)"
printf 'cpu.cores\t%s\n' "$(awk -F': ' '/^core id/{print $2}' $P/cpuinfo 2>/dev/null | sort -u | wc -l)"
grep -qm1 ' hypervisor' $P/cpuinfo 2>/dev/null && printf 'cpu.virtual\t1\n'
kv cpu.mhz_min $S/devices/system/cpu/cpu0/cpufreq/cpuinfo_min_freq
kv cpu.mhz_max $S/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq
kv cpu.mhz_cur $S/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq
printf 'cpu.load\t%s\n' "$(r $P/loadavg)"
printf 'sys.uptime\t%s\n' "$(r $P/uptime)"

# --- memory ---
awk '/^(MemTotal|MemAvailable|Cached|SwapTotal|SwapFree):/{print "mem." $1 "\t" $2}' $P/meminfo 2>/dev/null | tr -d ':'

# --- gpu ---
# Model names are resolved from the HOST'S OWN pci.ids when it has one (mounted
# read-only at /hostpci), rather than shipping a megabyte of lookup tables in a
# backup tool. Falls back to vendor + id when the host has no database.
pciname() {
  [ -f /hostpci ] || return
  awk -v v="$1" -v d="$2" '
    substr($0,1,1)=="#" {next}
    substr($0,1,1)!="\t" {inv = (substr($0,1,4)==v); next}
    inv && substr($0,2,1)!="\t" { if (substr($0,2,4)==d) {print substr($0,8); exit} }
  ' /hostpci 2>/dev/null | head -c 120
}
# Iterate real CARDS only. /sys/class/drm also holds one entry per display
# CONNECTOR (card0-DP-1, card0-HDMI-A-1, ...), and each of those has a "device"
# symlink back to the same PCI device — so globbing card* enumerated a GPU per
# monitor output. Connector names always contain a dash; card nodes never do.
for c in $S/class/drm/card*; do
  cn=$(basename "$c")
  case "$cn" in *-*) continue;; esac
  d="$c/device"
  [ -f "$d/vendor" ] || continue
  drv=$(readlink "$d/driver" 2>/dev/null | sed 's#.*/##')
  ven=$(r $d/vendor); dev=$(r $d/device)
  nm=$(pciname "$(echo $ven | sed 's/^0x//')" "$(echo $dev | sed 's/^0x//')")
  # Keyed by CARD, not by vendor+device: two identical cards in one machine are
  # two GPUs, and their sensors must not cross-contaminate.
  pci=$(readlink "$d" 2>/dev/null | sed 's#.*/##')
  printf 'gpu\t%s|%s|%s|%s|%s|%s\n' "$cn" "$ven" "$dev" "$drv" "$nm" "$pci"
done 2>/dev/null

# The NVIDIA proprietary driver publishes the exact marketing name here, which
# beats any database lookup. Free: /proc is already mounted.
for g in $P/driver/nvidia/gpus/*/information; do
  awk -F':[ \t]*' '/^Model:/{print "gpu.nvidia\t" $2; exit}' "$g" 2>/dev/null
done 2>/dev/null

# --- gpu telemetry ---
# Temperature, fan, power and (on AMD) utilisation + VRAM come from the DRIVER'S
# OWN hwmon node, not from nvidia-smi: the proprietary NVIDIA driver registers
# one just like amdgpu/nouveau/i915 do, so a plain unprivileged sidecar can read
# it with no container toolkit and no device passthrough.
for c in $S/class/drm/card*; do
  cn=$(basename "$c")
  case "$cn" in *-*) continue;; esac
  d="$c/device"
  [ -f "$d/vendor" ] || continue
  for h in $d/hwmon/hwmon*; do
    [ -d "$h" ] || continue
    printf 'gpu.sensor\t%s|temp|%s\n' "$cn" "$(r $h/temp1_input)"
    printf 'gpu.sensor\t%s|fan|%s\n'  "$cn" "$(r $h/fan1_input)"
    # Older cards report average power, newer AMD (RDNA3) and some NVIDIA report
    # instantaneous power instead. Reading only one left the newer ones blank.
    pw=$(r $h/power1_average)
    [ -n "$pw" ] || pw=$(r $h/power1_input)
    printf 'gpu.sensor\t%s|power|%s\n' "$cn" "$pw"
  done
  # amdgpu publishes real utilisation and VRAM; NVIDIA's proprietary driver
  # publishes neither outside nvidia-smi, so those simply come back empty.
  printf 'gpu.sensor\t%s|busy|%s\n'      "$cn" "$(r $d/gpu_busy_percent)"
  printf 'gpu.sensor\t%s|vramtotal|%s\n' "$cn" "$(r $d/mem_info_vram_total)"
  printf 'gpu.sensor\t%s|vramused|%s\n'  "$cn" "$(r $d/mem_info_vram_used)"
done 2>/dev/null

# --- hwmon sweep ---
# Where the real sensors live: coretemp/k10temp (CPU package + per-core), nvme
# (drive), amdgpu/nvidia (GPU), plus board sensors. thermal_zone alone misses
# almost all of these, which is why a laptop could show ACPI zones and no CPU
# temperature at all.
for h in $S/class/hwmon/hwmon*; do
  nm=$(r $h/name)
  [ -n "$nm" ] || continue
  for tf in $h/temp*_input; do
    [ -f "$tf" ] || continue
    lbl=$(r "$(echo "$tf" | sed 's/_input$/_label/')")
    printf 'hwmon\t%s|%s|%s\n' "$nm" "$lbl" "$(r $tf)"
  done
done 2>/dev/null

# --- network interfaces ---
# Only PHYSICAL links. The test is structural, not a name blacklist: a real NIC
# has a "device" symlink to its PCI/USB/MMIO parent, while every virtual
# interface — tailscale0, wg0, tun0, docker0, br-*, veth*, bonds, VLANs — has
# none. That is what keeps a VPN tunnel off a page about the machine, without a
# list of product names to keep chasing.
for i in $S/class/net/*; do
  n=$(basename "$i")
  case "$n" in lo) continue;; esac
  [ -e "$i/device" ] || continue
  printf 'net\t%s|%s|%s\n' "$n" "$(r $i/speed)" "$(r $i/operstate)"
done 2>/dev/null

# --- thermal zones + hwmon ---
for t in $S/class/thermal/thermal_zone*; do
  printf 'therm\t%s|%s\n' "$(r $t/type)" "$(r $t/temp)"
done 2>/dev/null

# --- battery / AC (laptops) ---
for b in $S/class/power_supply/*; do
  ty=$(r "$b/type")
  [ "$ty" = "Battery" ] && printf 'battery\t%s\n' "$(r $b/capacity)"
  [ "$ty" = "Mains" ] && printf 'ac\t%s\n' "$(r $b/online)"
done 2>/dev/null

# --- os ---
awk -F= '/^PRETTY_NAME=/{gsub(/"/,"",$2); print "os.name\t" $2}' /hostosrelease 2>/dev/null
printf 'os.kernel\t%s\n' "$(r $P/sys/kernel/osrelease)"

# --- RATE SAMPLES: first read, wait, second read. Emitted as a/b pairs. ---
sample() {
  pfx=$1
  awk -v p="$pfx" '/^cpu /{print p ".cpu\t" $2+$3+$4+$6+$7+$8 "\t" $5}' $P/stat 2>/dev/null
  awk -v p="$pfx" '{print p ".disk\t" $3 "\t" $6 "\t" $10}' $P/diskstats 2>/dev/null
  for i in $S/class/net/*; do
    n=$(basename "$i")
    case "$n" in lo) continue;; esac
    [ -e "$i/device" ] || continue
    printf '%s.net\t%s\t%s\t%s\n' "$pfx" "$n" "$(r $i/statistics/rx_bytes)" "$(r $i/statistics/tx_bytes)"
  done 2>/dev/null
}
printf 'a.at\t%s\n' "$(date +%s)"
sample a
sleep SAMPLESECS
sample b
printf 'b.at\t%s\n' "$(date +%s)"

# --- block devices: LAST, because this is the section that can hang ---
# Reading a disk's model WAKES a spun-down drive. On a NAS with several sleeping
# bays that read blocks in uninterruptible I/O, which no timeout can interrupt
# (neither SIGTERM nor SIGKILL reaches a process in D state). So: emit the cheap
# attributes FIRST as their own line, then attempt the model separately. A hang
# then costs one model string, not the disk, and not the whole probe -- and
# everything above has already been written.
for b in $S/block/*; do
  n=$(basename "$b")
  case "$n" in loop*|ram*|dm-*|sr*|zram*|md*|*boot0|*boot1|*rpmb) continue;; esac
  sz=$(r "$b/size"); [ -z "$sz" ] && continue
  printf 'disk\t%s|%s|%s\n' "$n" "$sz" "$(r $b/queue/rotational)"
  printf 'disk.model\t%s|%s\n' "$n" "$(r $b/device/model)"
done 2>/dev/null

# --- filesystem usage ---
# Free space is a property of a FILESYSTEM, not a disk: /sys/block gives capacity
# and contains no usage information anywhere. The only source is statfs() on a
# mounted path, so the host tree is mounted READ-ONLY at /hostfs and df is run
# against it. df performs statfs only -- no file is opened or read.
#
# Only real block-backed filesystems are reported: the device must be under /dev,
# which drops tmpfs, overlay, squashfs and the container's own layers.
if [ -d /hostfs ]; then
  df -P -k 2>/dev/null | awk '
    NR>1 && $1 ~ /^\/dev\// && $6 ~ /^\/hostfs/ {
      mp = substr($6, 8)
      if (mp == "") mp = "/"
      print "fs\t" $1 "|" mp "|" $2 "|" $3 "|" $4
    }'
fi

# --- device nodes ---
# Recorded so a cross-host restore can tell whether the target actually has the
# hardware a container asks for (F94) — /dev/dri for a transcoding GPU, an NVIDIA
# node, a serial adapter. Bounded and shallow: the top level plus the two
# directories that matter, never a full walk of /dev.
if [ -d /hostfs/dev ]; then
  for d in /hostfs/dev/* /hostfs/dev/dri/* /hostfs/dev/bus/usb/*/*; do
    [ -e "$d" ] || continue
    printf 'dev\t%s\n' "$(echo "$d" | sed 's|^/hostfs||')"
  done 2>/dev/null | head -n 300
fi

printf 'probe.done\t1\n'
`

// buildProbeScript injects the sample window (kept out of the literal so the
// script stays a constant and the window stays tunable in one place).
func buildProbeScript() string {
	return strings.ReplaceAll(probeScript, "SAMPLESECS", strconv.Itoa(probeSampleSeconds))
}

// ProbeMachine runs the read-only host probe on a node and parses the result.
//
// The sidecar mounts ONLY /proc, /sys and /etc/os-release, all :ro, with no
// privileged flag and no added capabilities. It is labeled like every other
// DockBack sidecar so the orphan sweep can reclaim it if we die mid-probe.
func ProbeMachine(ctx context.Context, c *client.Client) (*MachineInfo, error) {
	// Pulling the sidecar image is a ONE-OFF that can legitimately take minutes on
	// a slow link, and it is not part of "reading this machine". Giving it its own
	// budget stops a first-ever probe on a node from being reported as a timeout
	// when it was really just fetching a few MB of Alpine.
	imgCtx, imgCancel := context.WithTimeout(ctx, 3*time.Minute)
	err := ensureSidecar(imgCtx, c)
	imgCancel()
	if err != nil {
		return nil, fmt.Errorf("machine probe image: %w", err)
	}
	// Generous, because this runs in the BACKGROUND and nothing waits on it: a NAS
	// waking four spun-down bays legitimately takes tens of seconds. Whatever has
	// been emitted by the deadline is still used, so the cap bounds the container's
	// lifetime rather than deciding success.
	//
	// NOTE for callers: pass a context that is NOT tied to an HTTP request. This
	// used to inherit the handler's 20s deadline, which both capped the budget
	// below and killed any probe still running when the request returned.
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// Mount sets, tried most-complete first. Each extra mount is a nice-to-have
	// that some hosts simply do not have (a minimal distro with no
	// /etc/os-release, a host without pciutils), and a bind to a missing source
	// fails the whole create — so we degrade one optional mount at a time rather
	// than losing the probe over a cosmetic field.
	base := []mount.Mount{
		{Type: mount.TypeBind, Source: "/proc", Target: "/hp", ReadOnly: true},
		{Type: mount.TypeBind, Source: "/sys", Target: "/hs", ReadOnly: true},
	}
	osRel := mount.Mount{Type: mount.TypeBind, Source: "/etc/os-release", Target: "/hostosrelease", ReadOnly: true}
	// The host's OWN PCI id database, when it has one. This is how a GPU becomes
	// "Atom/Celeron/Pentium Processor x5 Integrated Graphics" instead of
	// "0x22b0" — without DockBack shipping a megabyte of lookup tables that would
	// go stale. Read-only, single file, and entirely optional.
	pciCandidates := []string{
		"/usr/share/hwdata/pci.ids",
		"/usr/share/misc/pci.ids",
		"/usr/share/pci.ids",
	}

	// The host tree, read-only, so df can report free space (F105). Listed FIRST
	// and always optional: a host that refuses it still gets every other section.
	//
	// SECURITY: this does not widen what an attacker who owns DockBack can do —
	// the socket-proxy already permits container create, so any host path could
	// already be mounted. What it changes is what DockBack's OWN probe touches, so
	// it is read-only, short-lived, unprivileged, and used for statfs alone.
	hostfs := mount.Mount{Type: mount.TypeBind, Source: "/", Target: "/hostfs", ReadOnly: true}

	var attempts [][]mount.Mount
	for _, p := range pciCandidates {
		attempts = append(attempts, append(append([]mount.Mount{}, base...), osRel, hostfs,
			mount.Mount{Type: mount.TypeBind, Source: p, Target: "/hostpci", ReadOnly: true}))
	}
	attempts = append(attempts, append(append([]mount.Mount{}, base...), osRel, hostfs))
	for _, p := range pciCandidates {
		attempts = append(attempts, append(append([]mount.Mount{}, base...), osRel,
			mount.Mount{Type: mount.TypeBind, Source: p, Target: "/hostpci", ReadOnly: true}))
	}
	attempts = append(attempts, append(append([]mount.Mount{}, base...), osRel))
	attempts = append(attempts, base)

	var created container.CreateResponse
	for _, mounts := range attempts {
		created, err = c.ContainerCreate(ctx,
			&container.Config{
				Image:        sidecarRef(),
				Cmd:          []string{"/bin/sh", "-c", buildProbeScript()},
				AttachStdout: true, AttachStderr: true,
				Labels: sidecarLabels(),
			},
			&container.HostConfig{
				AutoRemove: false,
				// READ-ONLY host visibility. No privileged, no cap_add, no /dev/mem —
				// which is why DIMM-level detail (dmidecode) is deliberately out of scope.
				Mounts: mounts,
			},
			nil, nil, "")
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("machine probe: %w", err)
	}
	sidecarID := created.ID
	defer removeContainer(c, sidecarID)

	att, err := c.ContainerAttach(ctx, sidecarID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		return nil, fmt.Errorf("machine probe attach: %w", err)
	}
	defer att.Close()
	if err := c.ContainerStart(ctx, sidecarID, container.StartOptions{}); err != nil {
		return nil, fmt.Errorf("machine probe start: %w", err)
	}

	var stdout, stderr bytes.Buffer
	timedOut := false
	lw := &limitedWriter{w: &stdout, n: maxProbeBytes}
	_, copyErr := stdcopy.StdCopy(lw, &stderr, att.Reader)
	att.Close()

	waitCh, errCh := c.ContainerWait(context.Background(), sidecarID, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
	case e := <-errCh:
		if copyErr == nil {
			copyErr = e
		}
	case <-ctx.Done():
		// DO NOT discard what arrived. The script writes cheapest-first, so by the
		// time something late hangs (a NAS waking spun-down disks, typically) the
		// identity, CPU and memory sections are already on the wire. Throwing that
		// away turned a slow host into a blank page saying nothing could be read.
		timedOut = true
	}
	if copyErr != nil && stdout.Len() == 0 {
		return nil, copyErr
	}
	if stdout.Len() == 0 {
		if timedOut {
			return nil, fmt.Errorf("machine probe timed out before this host reported anything")
		}
		return nil, fmt.Errorf("machine probe returned nothing: %s", strings.TrimSpace(stderr.String()))
	}
	info := ParseMachineProbe(stdout.String())
	// A probe that was cut short still returns everything it managed to read —
	// but must never pretend to be a complete picture.
	if timedOut || !info.Complete {
		info.Partial = true
		info.Warnings = append(info.Warnings,
			"This host was slow to answer, so the reading below is incomplete — the sections that did respond are shown. Common on a NAS with disks in standby: waking them can block for longer than the probe waits.")
	}

	// F105: the proprietary NVIDIA driver publishes no hwmon node, so sysfs gives
	// us the card's identity and nothing else. nvidia-smi (NVML) is the only
	// source for its temperature, utilisation, VRAM and power — ask the host's own
	// copy of it, via a container the NVIDIA toolkit injects the driver into.
	//
	// Strictly best-effort and only attempted when an NVIDIA card is actually
	// present: a host without the Container Toolkit simply keeps the identity-only
	// view it already had, and the page says why.
	if HasNvidia(info.GPUs) && needsNvidiaTelemetry(info.GPUs) {
		if stats, nerr := ProbeNvidiaSMI(ctx, c); nerr == nil {
			MergeNvidiaStats(info.GPUs, stats)
		} else {
			info.Warnings = append(info.Warnings,
				"An NVIDIA GPU is present but its live telemetry (temperature, utilisation, VRAM) needs the NVIDIA Container Toolkit on this host — the driver alone publishes none of it to the kernel: "+nerr.Error())
		}
	}
	return info, nil
}

// needsNvidiaTelemetry reports whether any NVIDIA card is still missing live
// numbers, so a machine that somehow already has them is not probed twice.
func needsNvidiaTelemetry(gpus []MachineGPU) bool {
	for _, g := range gpus {
		if strings.EqualFold(g.VendorID, "0x10de") && g.TempC == 0 && g.UsagePct == 0 {
			return true
		}
	}
	return false
}
