package config

import (
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// Container-aware runtime tuning (F106).
//
// The Go runtime does NOT read cgroup limits. Two consequences that matter for a
// backup tool:
//
//   - The garbage collector sizes the heap from live-heap growth (GOGC=100 means
//     "grow to 2x live"), with no idea a cgroup ceiling exists. A backup that
//     builds a large volume file index can push the heap past the container's
//     memory limit, and the kernel OOM-kills the process MID-BACKUP. From the
//     outside that looks like DockBack crashed for no reason.
//   - GOMAXPROCS defaults to the number of HOST cores, not the CPU quota. On a
//     32-core host limited to 2 CPUs, Go runs 32 OS threads fighting over 2
//     cores' worth of time — measurably slower than just using 2.
//
// So raising a container's limits does nothing on its own; the runtime has to be
// told. This reads the cgroup (v2 and v1) and configures both.
//
// SECURITY: nothing here touches the security posture. Memory and CPU limits are
// resource-exhaustion guardrails, not a boundary — the boundary is non-root, a
// read-only root filesystem, all capabilities dropped, no-new-privileges, the
// seccomp profile and the socket-proxy allow-list, none of which are involved.
// Reads are of the container's OWN cgroup files, and nothing is written.

// memLimitHeadroom is the fraction of the container's memory limit the Go heap
// is allowed to target. The remainder covers the non-heap footprint the runtime
// can't account for — thread stacks, the zstd encoder's window buffers, and the
// kernel's page cache for the work directory.
//
// GOMEMLIMIT is a SOFT limit: crossing it makes the GC work harder, it never
// fails an allocation. So the failure mode of setting it too low is "slower",
// while the failure mode of not setting it at all is "killed mid-backup".
const memLimitHeadroom = 0.85

// TuneRuntime aligns the Go runtime with the container's cgroup limits and
// returns a human summary for the boot log. Safe on bare metal and on any host
// without cgroups: it simply finds nothing and changes nothing.
//
// An explicitly-set GOMEMLIMIT or GOMAXPROCS always wins — an operator who has
// tuned by hand is not second-guessed.
func TuneRuntime() string {
	var notes []string

	if os.Getenv("GOMEMLIMIT") == "" {
		if limit, ok := cgroupMemoryLimit(); ok {
			target := int64(float64(limit) * memLimitHeadroom)
			debug.SetMemoryLimit(target)
			notes = append(notes, "heap target "+humanMB(target)+" of "+humanMB(limit)+" container memory")
		}
	}

	if os.Getenv("GOMAXPROCS") == "" {
		if quota, ok := cgroupCPUQuota(); ok && quota > 0 && quota < runtime.NumCPU() {
			runtime.GOMAXPROCS(quota)
			notes = append(notes, "GOMAXPROCS "+strconv.Itoa(quota)+" (CPU quota; host has "+strconv.Itoa(runtime.NumCPU())+")")
		}
	}

	if len(notes) == 0 {
		return "runtime: no cgroup limits detected — using all host memory and " +
			strconv.Itoa(runtime.GOMAXPROCS(0)) + " CPU(s)"
	}
	return "runtime: " + strings.Join(notes, "; ")
}

// cgroupMemoryLimit returns the container's memory limit in bytes.
//
// Handles cgroup v2 ("max" means unlimited) and v1 (a huge sentinel near
// int64-max means unlimited). An unlimited cgroup returns ok=false so the caller
// leaves the GC alone.
func cgroupMemoryLimit() (int64, bool) {
	for _, p := range []string{
		"/sys/fs/cgroup/memory.max",                   // v2
		"/sys/fs/cgroup/memory/memory.limit_in_bytes", // v1
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "" || s == "max" {
			continue
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 {
			continue
		}
		// v1 reports "no limit" as a value near the top of int64. Anything above a
		// terabyte is not a real container limit; treat it as unlimited rather
		// than pinning the heap to an absurd number.
		if v > 1<<40 {
			continue
		}
		return v, true
	}
	return 0, false
}

// cgroupCPUQuota returns the CPU quota rounded UP to whole cores.
//
// Rounding up matters: a 1.5-CPU quota with GOMAXPROCS=1 would leave half a core
// unused, whereas GOMAXPROCS=2 lets the scheduler actually reach the quota.
func cgroupCPUQuota() (int, bool) {
	// cgroup v2: "<quota> <period>", or "max <period>" when unlimited.
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(strings.TrimSpace(string(b)))
		if len(f) == 2 && f[0] != "max" {
			quota, err1 := strconv.ParseInt(f[0], 10, 64)
			period, err2 := strconv.ParseInt(f[1], 10, 64)
			if err1 == nil && err2 == nil && period > 0 && quota > 0 {
				return ceilDiv(quota, period), true
			}
		}
	}
	// cgroup v1: separate quota/period files; quota -1 means unlimited.
	qb, err1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	pb, err2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err1 == nil && err2 == nil {
		quota, e1 := strconv.ParseInt(strings.TrimSpace(string(qb)), 10, 64)
		period, e2 := strconv.ParseInt(strings.TrimSpace(string(pb)), 10, 64)
		if e1 == nil && e2 == nil && quota > 0 && period > 0 {
			return ceilDiv(quota, period), true
		}
	}
	return 0, false
}

func ceilDiv(a, b int64) int {
	n := int((a + b - 1) / b)
	if n < 1 {
		n = 1
	}
	return n
}

func humanMB(b int64) string {
	return strconv.FormatInt(b/(1<<20), 10) + " MiB"
}
