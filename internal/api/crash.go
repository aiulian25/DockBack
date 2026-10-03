package api

import (
	"strconv"
	"time"

	"dockback/internal/dockercli"
	"dockback/internal/notify"
)

// Crash & OOM detection (F24).
//
// DockBack already watches each node's Docker event stream (for inventory
// refresh and the F7 pre-change snapshot). This adds a first-class, severity-
// routed WARNING when a container DockBack protects dies badly:
//
//   - an "oom" event  -> the kernel killed it for running out of memory
//   - a "die" event with a non-zero exit code -> it crashed
//
// A clean stop (exit 0) is silent. Alerts are THROTTLED per container so a
// crash-loop pages you once per window, not on every restart. The "optionally
// protect" half of the feature needs no code here: a die already qualifies for
// the F7 autosnap path, so a protected container gets its pre-change snapshot
// from maybeAutosnap on the same event.

// oomCorrelationWindow bounds how long after an "oom" event the trailing "die"
// (exit 137) is treated as the same kill. Docker emits them back-to-back, so a
// few seconds is ample; kept generous for a loaded host.
const oomCorrelationWindow = 30 * time.Second

// classifyCrash decides whether a change event is a crash/OOM worth alerting on,
// returning the notification kind, title and message. Pure (no Docker/DB/clock)
// so it is unit-testable and cheap on the hot event path.
func classifyCrash(ev dockercli.ChangeEvent) (kind, title, message string, ok bool) {
	if ev.Type != "container" || ev.Name == "" {
		return "", "", "", false
	}
	switch {
	case ev.OOMKilled || ev.Action == "oom":
		return notify.KindContainerOOM,
			"Out of memory: " + ev.Name,
			"Container \"" + ev.Name + "\" was killed for running out of memory. It may crash-loop into a bad state — review its memory limit and recent changes.",
			true
	case ev.Action == "die" && ev.ExitCode != 0:
		return notify.KindContainerCrashed,
			"Container crashed: " + ev.Name,
			"Container \"" + ev.Name + "\" crashed (exit " + strconv.Itoa(ev.ExitCode) + "). Check its logs — if it keeps restarting it may be looping into a bad state.",
			true
	}
	return "", "", "", false
}

// maybeCrashAlert raises a throttled crash/OOM warning for a WATCHED container.
// Called from the event-stream watcher, so it stays cheap: it early-returns for
// the common clean-stop/other events before touching the DB.
func (s *Server) maybeCrashAlert(nodeID string, ev dockercli.ChangeEvent) {
	kind, title, message, ok := classifyCrash(ev)
	if !ok {
		return
	}
	// Only alert for a container DockBack actually watches (protects), so a random
	// throwaway container's crash doesn't page the user.
	if !s.isWatchedContainer(nodeID, ev.Name) {
		return
	}

	// OOM correlation: an OOM kill emits "oom" then "die" (exit 137). Record the
	// OOM, and when its trailing die arrives within the window, swallow the crash
	// alert so one kill = one notification (the more specific OOM one).
	key := nodeID + "\x00" + ev.Name
	now := time.Now()
	s.crashMu.Lock()
	if kind == notify.KindContainerOOM {
		s.recentOOM[key] = now
		for k, t := range s.recentOOM { // opportunistic prune; the map stays tiny
			if now.Sub(t) > oomCorrelationWindow {
				delete(s.recentOOM, k)
			}
		}
	} else if oomAt, seen := s.recentOOM[key]; seen && now.Sub(oomAt) <= oomCorrelationWindow {
		delete(s.recentOOM, key)
		s.crashMu.Unlock()
		return // this die is the OOM's die — already reported as OOM
	}
	s.crashMu.Unlock()

	// If the container also has pre-change protection on, tell the user a snapshot
	// is being taken (the F7 autosnap path fires from the same die event).
	if s.loadAutosnap()[autosnapKey(nodeID, ev.Name)] {
		message += " A protective \"pre-change\" snapshot is being taken."
	}
	// Throttle per container (dedup=key) so a crash-loop alerts once per window.
	s.notifyThrottled(kind, key, title, message, crashAlertCooldown)
}

// isWatchedContainer reports whether DockBack protects this container — it has
// pre-change snapshots enabled (F7) or at least one successful backup on record.
// Those are the containers whose failure the user has signalled they care about;
// unprotected throwaway containers stay quiet.
func (s *Server) isWatchedContainer(nodeID, name string) bool {
	if s.loadAutosnap()[autosnapKey(nodeID, name)] {
		return true
	}
	if prot, err := s.store.ProtectedTargets(nodeID); err == nil {
		if _, ok := prot[name]; ok {
			return true
		}
	}
	return false
}
