package dockercli

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/docker/docker/client"
)

// HostInfo describes a node's Docker host (PLAN §5.4 node-detail header).
type HostInfo struct {
	Name          string `json:"name"` // hostname
	OS            string `json:"os"`   // e.g. "Ubuntu 24.04.4 LTS"
	OSVersion     string `json:"os_version"`
	Kernel        string `json:"kernel"`
	Arch          string `json:"arch"` // e.g. "x86_64"
	DockerVersion string `json:"docker_version"`
	NCPU          int    `json:"ncpu"`
	MemTotal      int64  `json:"mem_total"`
}

// Info fetches the node's host details from its Docker daemon.
func Info(ctx context.Context, c *client.Client) (*HostInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	i, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	return &HostInfo{
		Name:          i.Name,
		OS:            i.OperatingSystem,
		OSVersion:     i.OSVersion,
		Kernel:        i.KernelVersion,
		Arch:          i.Architecture,
		DockerVersion: i.ServerVersion,
		NCPU:          i.NCPU,
		MemTotal:      i.MemTotal,
	}, nil
}

// statsJSON is a minimal projection of the Docker stats payload (decoded into
// our own struct to stay resilient to API type churn).
type statsJSON struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage  uint64   `json:"total_usage"`
		PercpuUsage []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	SystemUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs  uint32 `json:"online_cpus"`
}

// NodeStats aggregates CPU% and memory usage across the given running container
// IDs, plus the host memory total (PLAN §5.4 hub(5) CPU/MEM bars). It is
// best-effort: per-container errors are skipped rather than failing the card.
func NodeStats(ctx context.Context, c *client.Client, runningIDs []string) (cpuPct float64, memUsed, memTotal, netRx, netTx int64) {
	if info, err := c.Info(ctx); err == nil {
		memTotal = info.MemTotal
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, 8) // cap concurrent stats reads (PLAN §4.13)
	)
	for _, id := range runningIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			resp, err := c.ContainerStats(cctx, id, false) // stream=false: daemon includes precpu
			if err != nil {
				return
			}
			var s statsJSON
			dec := json.NewDecoder(resp.Body)
			derr := dec.Decode(&s)
			resp.Body.Close()
			if derr != nil {
				return
			}

			cpu := calcCPU(&s)
			mem := calcMem(&s)
			var rx, tx uint64
			for _, n := range s.Networks {
				rx += n.RxBytes
				tx += n.TxBytes
			}
			mu.Lock()
			cpuPct += cpu
			memUsed += int64(mem)
			netRx += int64(rx)
			netTx += int64(tx)
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return cpuPct, memUsed, memTotal, netRx, netTx
}

func calcCPU(s *statsJSON) float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	if sysDelta <= 0 || cpuDelta < 0 {
		return 0
	}
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpus == 0 {
		cpus = 1
	}
	return (cpuDelta / sysDelta) * cpus * 100.0
}

// calcMem mirrors `docker stats`: usage minus inactive file cache.
func calcMem(s *statsJSON) uint64 {
	usage := s.MemoryStats.Usage
	if v, ok := s.MemoryStats.Stats["inactive_file"]; ok && v < usage { // cgroup v2
		return usage - v
	}
	if v, ok := s.MemoryStats.Stats["total_inactive_file"]; ok && v < usage { // cgroup v1
		return usage - v
	}
	return usage
}
