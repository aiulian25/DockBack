package api

import (
	"fmt"
	"sort"
	"time"

	"dockback/internal/notify"
	"dockback/internal/store"
)

// Backup anomaly detection (F28). After a successful backup, its wall-clock
// duration and archive size are compared against the median of the same
// container's recent successful backups. A run that is sharply larger/slower than
// the norm is an early warning — a runaway volume, a media library that ballooned,
// or a degrading/slow disk — surfaced (throttled, warning severity) BEFORE a
// destination actually fills. Cheap: one catalog read, all in-memory.

const (
	anomalySamples    = 8   // most-recent prior successful backups to compare against
	anomalyMinSamples = 3   // need at least this many priors to judge (else skip)
	defaultAnomalyF   = 3   // deviation factor (Nx the median) that triggers a warning
	minAnomalyF       = 2   // clamp: below 2x is too noisy to be useful
	maxAnomalyF       = 100 // clamp: generous ceiling
)

// anomalyFactor is the effective deviation factor (setting alert.anomaly_factor,
// default 3, clamp 2..100 — F28, mirrors the F18 int-setting pattern). A run more
// than this many times its usual duration/size raises the warning.
func (s *Server) anomalyFactor() int {
	return clampInt(s.settingInt("alert.anomaly_factor", defaultAnomalyF), minAnomalyF, maxAnomalyF)
}

// anomalyVerdict is the pure decision: given the newest run's duration/size and the
// prior samples for the same container, report whether each metric is anomalous
// (newest > factor x median) and the median it exceeded. A metric with fewer than
// anomalyMinSamples priors is never flagged (nothing trustworthy to compare).
type anomalyVerdict struct {
	durAnom     bool
	sizeAnom    bool
	medianDurMs int64
	medianSize  int64
}

func judgeAnomaly(newDurMs, newSize int64, priorDurs, priorSizes []int64, factor int) anomalyVerdict {
	var v anomalyVerdict
	f := float64(factor)
	if len(priorDurs) >= anomalyMinSamples && newDurMs > 0 {
		m := medianInt64(priorDurs)
		v.medianDurMs = m
		if m > 0 && float64(newDurMs) > f*float64(m) {
			v.durAnom = true
		}
	}
	if len(priorSizes) >= anomalyMinSamples && newSize > 0 {
		m := medianInt64(priorSizes)
		v.medianSize = m
		if m > 0 && float64(newSize) > f*float64(m) {
			v.sizeAnom = true
		}
	}
	return v
}

// medianInt64 returns the median of xs (0 for empty). Does not mutate the input.
func medianInt64(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// checkBackupAnomaly runs after a successful backup: it gathers the container's
// recent prior successes, judges duration/size drift, and (throttled per
// container) raises one warning naming what drifted (F28). No-op with too few
// priors, so a container never alerts on its first few backups.
func (s *Server) checkBackupAnomaly(b *store.Backup) {
	if b == nil || b.Status != "success" || b.TargetName == "" {
		return
	}
	list, err := s.store.ListBackupsForTarget(b.NodeID, b.TargetName, 5000)
	if err != nil {
		return
	}
	// Newest-first: collect up to anomalySamples prior successes for this container,
	// excluding the run we're judging. Pre-F28 rows (duration 0) are skipped per
	// metric so the size check still works on old history.
	var priorDurs, priorSizes []int64
	for _, x := range list {
		if x.ID == b.ID || x.Status != "success" {
			continue
		}
		if len(priorSizes) >= anomalySamples {
			break
		}
		if x.DurationMs > 0 {
			priorDurs = append(priorDurs, x.DurationMs)
		}
		if x.SizeBytes > 0 {
			priorSizes = append(priorSizes, x.SizeBytes)
		}
	}

	v := judgeAnomaly(b.DurationMs, b.SizeBytes, priorDurs, priorSizes, s.anomalyFactor())
	if !v.durAnom && !v.sizeAnom {
		return
	}

	var what string
	switch {
	case v.durAnom && v.sizeAnom:
		what = fmt.Sprintf("took %s (usually ~%s) AND grew to %s (usually ~%s)",
			humanDuration(b.DurationMs), humanDuration(v.medianDurMs), humanBytes(b.SizeBytes), humanBytes(v.medianSize))
	case v.durAnom:
		what = fmt.Sprintf("took %s — much longer than its usual ~%s", humanDuration(b.DurationMs), humanDuration(v.medianDurMs))
	default:
		what = fmt.Sprintf("grew to %s — much larger than its usual ~%s", humanBytes(b.SizeBytes), humanBytes(v.medianSize))
	}
	s.notifyThrottled(notify.KindBackupAnomaly, b.NodeID+"\x00"+b.TargetName,
		fmt.Sprintf("Unusual backup: %s", b.TargetName),
		fmt.Sprintf("The latest backup of %s %s. Check for a runaway volume (a media library that ballooned) or a slow/degrading disk before it becomes a problem.", b.TargetName, what),
		anomalyCooldown)
}

// humanDuration formats a millisecond duration for an alert (e.g. "40s", "3m20s").
func humanDuration(ms int64) string {
	if ms <= 0 {
		return "0s"
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	if d < time.Hour {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}
