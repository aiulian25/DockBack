package dockercli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// NVIDIA GPU telemetry (F105).
//
// WHY THIS EXISTS AT ALL: unlike amdgpu, nouveau and i915, the proprietary
// NVIDIA driver registers NO hwmon node. That is why `sensors` cannot read an
// NVIDIA GPU's temperature either — the numbers live only behind NVML, and
// `nvidia-smi` is the tool that reads it. So sysfs alone can give us the card's
// identity and nothing else: no temperature, no utilisation, no VRAM.
//
// HOW: run `nvidia-smi` inside a throwaway container that the NVIDIA Container
// Toolkit has injected the driver into. The toolkit mounts the HOST's own
// nvidia-smi and libraries, so nothing NVIDIA-specific is shipped or installed
// by DockBack, and the container still gets no privileges beyond the GPU device.
//
// TWO REQUIREMENTS, both on the host, and both reported honestly when missing:
//
//   1. The NVIDIA Container Toolkit must be installed. Having `nvidia-smi` on
//      the host is NOT sufficient — that is the driver; the toolkit is what lets
//      a container see the GPU. Without it, container creation is refused and we
//      simply report no telemetry.
//   2. A glibc base image. The injected nvidia-smi is dynamically linked against
//      glibc, so it cannot run on the musl-based Alpine sidecar used everywhere
//      else — hence the separate, small Debian image below.
//
// Everything here is strictly best-effort: any failure leaves the card showing
// the identity sysfs already gave us, exactly as before.

// nvidiaProbeRefs are glibc bases, in preference order. The toolkit injects the
// driver and nvidia-smi, so all that is needed is a libc the injected binary can
// link against — deliberately NOT a CUDA image, which would be gigabytes.
//
// A list rather than one image because an already-present base is free: if a
// host has any of these, nothing is pulled at all.
var nvidiaProbeRefs = []string{"debian:stable-slim", "debian:12-slim", "ubuntu:24.04", "ubuntu:22.04"}

// nvidiaQueries are field sets tried in order, richest first.
//
// This progression is what makes the probe work across ALL cards and driver
// generations. nvidia-smi rejects the ENTIRE query if it does not recognise a
// single field, so asking for everything and hoping would return nothing at all
// on an older driver, a Tesla without a fan, or a vGPU without power telemetry.
// Each fallback drops the fields most likely to be unsupported and keeps the
// ones every generation has.
//
// The parser does not care which set succeeded: it reads the bus id and name
// positionally and matches the remaining columns by the header nvidia-smi is
// asked for, so a short row simply leaves fields absent.
var nvidiaQueries = [][]string{
	{"pci.bus_id", "name", "temperature.gpu", "utilization.gpu", "memory.total", "memory.used", "power.draw", "fan.speed"},
	{"pci.bus_id", "name", "temperature.gpu", "utilization.gpu", "memory.total", "memory.used", "power.draw"},
	{"pci.bus_id", "name", "temperature.gpu", "utilization.gpu", "memory.total", "memory.used"},
	{"pci.bus_id", "name", "temperature.gpu", "memory.total", "memory.used"},
	{"pci.bus_id", "name", "temperature.gpu"},
	{"pci.bus_id", "name"},
}

// NvidiaStat is one GPU as nvidia-smi reports it. Every numeric field is
// optional: nvidia-smi prints "[N/A]" for anything a card doesn't support (a
// laptop GPU with no controllable fan, for instance), and that must stay absent
// rather than becoming a zero.
type NvidiaStat struct {
	BusID    string
	Name     string
	TempC    float64
	UsagePct float64
	MemTotal int64 // bytes
	MemUsed  int64 // bytes
	PowerW   float64
	FanPct   int
}

// ProbeNvidiaSMI asks the host's own nvidia-smi for live GPU telemetry.
//
// Works across driver generations by trying progressively smaller field sets:
// nvidia-smi rejects the whole query if it does not recognise one field, so a
// single unsupported column on an older card would otherwise yield nothing.
func ProbeNvidiaSMI(ctx context.Context, c *client.Client) ([]NvidiaStat, error) {
	// Any already-present glibc base is free; only pull if the host has none.
	img, err := ensureImageAvailable(ctx, c, nvidiaProbeRefs...)
	if err != nil {
		return nil, fmt.Errorf("nvidia probe needs a glibc base image: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	var lastErr error
	for _, fields := range nvidiaQueries {
		stats, qerr := runNvidiaQuery(ctx, c, img, fields)
		if qerr == nil && len(stats) > 0 {
			return stats, nil
		}
		if qerr != nil {
			lastErr = qerr
		}
		// A create/permission failure will fail identically for every field set,
		// so stop rather than spawning six doomed containers.
		if errors.Is(qerr, errNoGPURuntime) {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("nvidia-smi produced no usable output")
	}
	return nil, lastErr
}

// errNoGPURuntime means the host cannot expose a GPU to a container at all —
// the NVIDIA Container Toolkit is missing. Distinguished so the caller stops
// retrying and can say something actionable.
var errNoGPURuntime = errors.New("no GPU-capable container runtime on this host (NVIDIA Container Toolkit not installed?)")

// runNvidiaQuery executes ONE field set in a throwaway container.
func runNvidiaQuery(ctx context.Context, c *client.Client, img string, fields []string) ([]NvidiaStat, error) {
	cfg := &container.Config{
		Image:        img,
		Cmd:          []string{"nvidia-smi", "--query-gpu=" + strings.Join(fields, ","), "--format=csv,noheader,nounits"},
		AttachStdout: true, AttachStderr: true, Labels: sidecarLabels(),
		// The toolkit reads these: expose every GPU, but request only the
		// "utility" capability — nvidia-smi and NVML, not compute or graphics. The
		// container gets the least it can while still being able to ask.
		Env: []string{"NVIDIA_VISIBLE_DEVICES=all", "NVIDIA_DRIVER_CAPABILITIES=utility"},
	}
	// Two ways to request a GPU, newest first. DeviceRequests is the modern
	// `--gpus all`; Runtime "nvidia" is the older toolkit registration. Trying
	// both is what makes this work on old and new Docker installs alike.
	attempts := []*container.HostConfig{
		{AutoRemove: false, Resources: container.Resources{DeviceRequests: []container.DeviceRequest{
			{Driver: "nvidia", Count: -1, Capabilities: [][]string{{"gpu"}}},
		}}},
		{AutoRemove: false, Runtime: "nvidia"},
	}

	var created container.CreateResponse
	var err error
	for _, hc := range attempts {
		created, err = c.ContainerCreate(ctx, cfg, hc, nil, nil, "")
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoGPURuntime, err)
	}
	defer removeContainer(c, created.ID)

	att, aerr := c.ContainerAttach(ctx, created.ID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if aerr != nil {
		return nil, fmt.Errorf("nvidia probe attach: %w", aerr)
	}
	defer att.Close()
	if serr := c.ContainerStart(ctx, created.ID, container.StartOptions{}); serr != nil {
		return nil, fmt.Errorf("nvidia probe start: %w", serr)
	}

	var stdout, stderr bytes.Buffer
	lw := &limitedWriter{w: &stdout, n: 1 << 20}
	_, copyErr := stdcopy.StdCopy(lw, &stderr, att.Reader)
	att.Close()

	waitCh, errCh := c.ContainerWait(context.Background(), created.ID, container.WaitConditionNotRunning)
	select {
	case <-waitCh:
	case e := <-errCh:
		if copyErr == nil {
			copyErr = e
		}
	case <-ctx.Done():
		return nil, errors.New("nvidia probe timed out")
	}
	if copyErr != nil {
		return nil, copyErr
	}
	stats := ParseNvidiaSMI(stdout.String(), fields)
	if len(stats) == 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "no rows returned"
		}
		return nil, fmt.Errorf("nvidia-smi: %s", msg)
	}
	return stats, nil
}

// ParseNvidiaSMI parses `--format=csv,noheader,nounits` output for a given field
// list. Pure, so every shape of real output is testable without a GPU.
//
// Two things make this work across all cards rather than just the one in front
// of us:
//
//   - Columns are read by the FIELD NAME they were requested under, not by a
//     hardcoded position, so a reduced fallback query parses correctly.
//   - The GPU NAME is reassembled from the middle columns. Some cards report a
//     name containing a comma ("NVIDIA RTX A4000, Laptop GPU" on certain OEM
//     builds), which a naive comma split would shred into fields.
//
// nvidia-smi prints "[N/A]" / "[Not Supported]" for anything a card doesn't
// report. Those become ABSENT values, never zero: a passively-cooled Tesla
// reports no fan, and "0 %" would be a lie.
func ParseNvidiaSMI(out string, fields []string) []NvidiaStat {
	stats := []NvidiaStat{}
	if len(fields) < 2 {
		return stats
	}
	// Everything after the name is a single-token numeric column, so the name is
	// whatever is left in the middle once those are accounted for.
	numeric := len(fields) - 2

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < len(fields) {
			continue // a short row means the query failed; skip rather than misread
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		s := NvidiaStat{BusID: parts[0]}
		if s.BusID == "" {
			continue
		}
		// Name = everything between the bus id and the trailing numeric columns.
		nameEnd := len(parts) - numeric
		s.Name = strings.TrimSpace(strings.Join(parts[1:nameEnd], ", "))

		for i, fname := range fields[2:] {
			raw := parts[nameEnd+i]
			switch fname {
			case "temperature.gpu":
				s.TempC = nvNum(raw)
			case "utilization.gpu":
				s.UsagePct = nvNum(raw)
			case "memory.total":
				s.MemTotal = int64(nvNum(raw)) * 1024 * 1024 // MiB with --nounits
			case "memory.used":
				s.MemUsed = int64(nvNum(raw)) * 1024 * 1024
			case "power.draw":
				s.PowerW = nvNum(raw)
			case "fan.speed":
				s.FanPct = int(nvNum(raw))
			}
		}
		stats = append(stats, s)
	}
	return stats
}

// nvNum parses a value, treating nvidia-smi's not-available markers as zero —
// which callers then interpret as "absent" rather than storing.
func nvNum(s string) float64 {
	s = strings.TrimSpace(s)
	// CSV output brackets its markers ("[N/A]", "[Not Supported]"), but not every
	// driver build does — some print them bare. Accept both rather than letting a
	// stray "N/A" parse as garbage.
	if s == "" || strings.HasPrefix(s, "[") {
		return 0
	}
	switch strings.ToLower(s) {
	case "n/a", "not supported", "unknown", "-":
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// normalizeBusID makes two spellings of the same PCI address comparable.
//
// nvidia-smi prints an 8-digit domain (00000000:01:00.0) while sysfs uses 4
// (0000:01:00.0), so a plain string compare never matches. The DOMAIN is kept
// (with leading zeros stripped) rather than discarded: large servers really do
// have multiple PCI domains, and dropping it could make two distinct GPUs look
// like the same card.
func normalizeBusID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	parts := strings.Split(s, ":")
	if len(parts) == 0 {
		return s
	}
	// Strip leading zeros from the domain, keeping at least one digit.
	dom := strings.TrimLeft(parts[0], "0")
	if dom == "" {
		dom = "0"
	}
	parts[0] = dom
	return strings.Join(parts, ":")
}

// MergeNvidiaStats folds nvidia-smi telemetry onto the cards sysfs found.
//
// Matching is by PCI bus address, not enumeration order: on a hybrid laptop the
// DRM card index and the nvidia-smi index need not agree, and guessing would
// attribute a discrete GPU's temperature to the integrated one.
func MergeNvidiaStats(gpus []MachineGPU, stats []NvidiaStat) {
	for _, st := range stats {
		want := normalizeBusID(st.BusID)
		for i := range gpus {
			if normalizeBusID(gpus[i].PCIAddr) != want {
				continue
			}
			if st.Name != "" {
				gpus[i].Name = st.Name // nvidia-smi's name is the authoritative one
			}
			if st.TempC > 0 {
				gpus[i].TempC = st.TempC
			}
			// 0 % is a REAL reading on an idle GPU, so it is assigned
			// unconditionally — unlike temperature or power, where 0 only ever
			// means the card didn't report.
			gpus[i].UsagePct = st.UsagePct
			if st.PowerW > 0 {
				gpus[i].PowerW = st.PowerW
			}
			if st.MemTotal > 0 {
				gpus[i].VRAMTotal, gpus[i].VRAMUsed = st.MemTotal, st.MemUsed
			}
			if st.FanPct > 0 {
				gpus[i].FanPct = st.FanPct
			}
			gpus[i].Telemetry = "nvidia-smi"
			break
		}
	}
}

// HasNvidia reports whether any detected card is NVIDIA, so the (heavier)
// nvidia-smi probe is only ever attempted on a machine that could benefit.
func HasNvidia(gpus []MachineGPU) bool {
	for _, g := range gpus {
		if strings.EqualFold(g.VendorID, "0x10de") {
			return true
		}
	}
	return false
}
