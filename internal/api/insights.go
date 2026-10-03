package api

import (
	"net/http"
	"sort"
	"time"

	"dockback/internal/store"
)

// Insights aggregate (Fable-UI-UX B6): surfaces the time-series and derived data
// DockBack already computes but never shows — backup success/verified trend,
// destination capacity forecast + history, restore-drill confidence, and RPO
// adherence for critical databases. Everything is read from SQLite (the backup
// catalog, DestSample history, drills, critical settings) with NO Docker or
// network probes, so it's cheap enough to load on demand without degrading the app.

type insightsFleet struct {
	SuccessRate     float64 `json:"backup_success_rate"`
	Backups30d      int     `json:"backups_30d"`
	BackupsVerified int     `json:"backups_verified"`
}

type insightsDay struct {
	Day      int64 `json:"day"` // unix seconds at UTC day start
	Total    int   `json:"total"`
	Verified int   `json:"verified"`
	Failed   int   `json:"failed"`
}

type insightsDest struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	Type           string             `json:"type"`
	TotalBytes     uint64             `json:"total_bytes"`
	UsedBytes      uint64             `json:"used_bytes"`
	DaysToFull     int64              `json:"days_to_full"`
	FillDate       int64              `json:"fill_date"`
	GrowthPerMonth int64              `json:"growth_bytes_per_month"`
	History        []store.DestSample `json:"history"`
}

type insightsDrill struct {
	BackupID string `json:"backup_id"`
	OK       bool   `json:"ok"`
	RanAt    int64  `json:"ran_at"`
	Detail   string `json:"detail"`
	Target   string `json:"target"`    // container/service name the backup is of
	Stack    string `json:"stack"`     // compose project, if any
	NodeName string `json:"node_name"` // node the backup lives on
}

type insightsDrills struct {
	Total  int             `json:"total"`
	Passed int             `json:"passed"`
	Failed int             `json:"failed"`
	Recent []insightsDrill `json:"recent"`
}

type insightsRPOItem struct {
	Node            string `json:"node"`
	Container       string `json:"container"`
	TargetSeconds   int    `json:"target_seconds"`
	MeasuredSeconds int64  `json:"measured_seconds"` // -1 = no verified backup yet
	Met             bool   `json:"met"`
}

type insightsRPO struct {
	Total      int               `json:"total"`
	Meeting    int               `json:"meeting"`
	Breaching  int               `json:"breaching"`
	Containers []insightsRPOItem `json:"containers"`
}

// sizePoint is one backup's archive size at a point in time (F11 size trend).
type sizePoint struct {
	Ts    int64 `json:"ts"`
	Bytes int64 `json:"bytes"`
}

// insightsGrowth is one container's backup-size trend: the recent size points,
// its latest size, and a linear-fit growth rate per month (F11).
type insightsGrowth struct {
	Node           string      `json:"node"`
	NodeID         string      `json:"node_id"`
	Container      string      `json:"container"`
	Stack          string      `json:"stack,omitempty"`
	LatestBytes    int64       `json:"latest_bytes"`
	GrowthPerMonth int64       `json:"growth_bytes_per_month"` // signed; 0 = stable/too few points
	Points         []sizePoint `json:"points"`
}

type insightsResp struct {
	Fleet        insightsFleet    `json:"fleet"`
	BackupsDaily []insightsDay    `json:"backups_daily"`
	Destinations []insightsDest   `json:"destinations"`
	Drills       insightsDrills   `json:"drills"`
	RPO          insightsRPO      `json:"rpo"`
	TopGrowth    []insightsGrowth `json:"top_growth"`
}

// linearGrowthPerMonth fits backup size (y) over time (x, seconds) and returns
// the least-squares slope scaled to bytes/month (signed; negative = shrinking).
// Mirrors forecastFromSamples' math but on per-backup sizes. Returns 0 when there
// aren't enough points or span to trust a trend. Pure.
func linearGrowthPerMonth(pts []sizePoint) int64 {
	if len(pts) < forecastMinCount {
		return 0
	}
	if pts[len(pts)-1].Ts-pts[0].Ts < forecastMinSpan {
		return 0
	}
	var n, sx, sy, sxx, sxy float64
	x0 := float64(pts[0].Ts)
	for _, p := range pts {
		x := float64(p.Ts) - x0
		y := float64(p.Bytes)
		n++
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return 0
	}
	slope := (n*sxy - sx*sy) / denom // bytes per second
	return int64(slope * 30 * 24 * 3600)
}

// topGrowthFromBackups groups successful backups by (node, target), keeps the
// most recent ~12 points per container (oldest→newest), and ranks them by growth
// rate then latest size — surfacing runaway volumes before a destination fills.
// nodeName resolves a node id to its display name. Pure (no I/O).
func topGrowthFromBackups(list []*store.Backup, nodeName func(string) string, limit int) []insightsGrowth {
	type key struct{ node, target string }
	groups := map[key][]sizePoint{}
	stacks := map[key]string{}
	for _, b := range list {
		if b.Status != "success" || b.TargetName == "" {
			continue
		}
		k := key{b.NodeID, b.TargetName}
		if _, ok := groups[k]; !ok {
			stacks[k] = b.Stack
		}
		groups[k] = append(groups[k], sizePoint{Ts: b.CreatedAt, Bytes: b.SizeBytes})
	}
	out := make([]insightsGrowth, 0, len(groups))
	for k, pts := range groups {
		sort.Slice(pts, func(i, j int) bool { return pts[i].Ts < pts[j].Ts })
		if len(pts) > 12 {
			pts = pts[len(pts)-12:]
		}
		out = append(out, insightsGrowth{
			Node: nodeName(k.node), NodeID: k.node, Container: k.target, Stack: stacks[k],
			LatestBytes: pts[len(pts)-1].Bytes, GrowthPerMonth: linearGrowthPerMonth(pts), Points: pts,
		})
	}
	// Fastest-growing first, then largest as a tiebreak (stable ordering by name).
	sort.Slice(out, func(i, j int) bool {
		if out[i].GrowthPerMonth != out[j].GrowthPerMonth {
			return out[i].GrowthPerMonth > out[j].GrowthPerMonth
		}
		if out[i].LatestBytes != out[j].LatestBytes {
			return out[i].LatestBytes > out[j].LatestBytes
		}
		return out[i].Container < out[j].Container
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// handleInsights returns the aggregated trends/analytics for the Insights page (B6).
func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	nowTs := now.Unix()

	// --- Backup catalog: 30-day fleet rate + 90-day daily trend (one pass) ---
	// Slim projection (perf Fix 6): every consumer of `list` in this handler
	// reads scalars only (day counts, byID target/stack/node for drills,
	// topGrowthFromBackups size/time series) — never a JSON blob.
	list, _ := s.store.ListBackupSummaries("", 5000)
	cutoff30 := now.AddDate(0, 0, -30).Unix()
	cutoff90 := now.AddDate(0, 0, -90).Unix()
	dayMap := map[int64]*insightsDay{}
	var t30, v30 int
	for _, b := range list {
		if b.Status == "running" {
			continue
		}
		if b.CreatedAt >= cutoff30 {
			t30++
			if b.Verified == "verified" {
				v30++
			}
		}
		if b.CreatedAt >= cutoff90 {
			day := (b.CreatedAt / 86400) * 86400
			d := dayMap[day]
			if d == nil {
				d = &insightsDay{Day: day}
				dayMap[day] = d
			}
			d.Total++
			if b.Verified == "verified" {
				d.Verified++
			}
			if b.Status == "failed" {
				d.Failed++
			}
		}
	}
	daily := make([]insightsDay, 0, len(dayMap))
	for _, d := range dayMap {
		daily = append(daily, *d)
	}
	sort.Slice(daily, func(i, j int) bool { return daily[i].Day < daily[j].Day })
	rate := 100.0
	if t30 > 0 {
		rate = float64(v30) / float64(t30) * 100.0
	}

	// --- Destinations: current usage (newest sample) + forecast + history ---
	// Includes the primary backups volume (F42), sampled under a synthetic id, so
	// the volume every backup lands on appears in the capacity forecast alongside
	// the offsite destinations.
	dests, _ := s.store.ListDestinations()
	outDests := []insightsDest{}
	addDest := func(id, name, typ string) {
		samples, _ := s.store.DestSamples(id, nowTs-90*24*3600)
		var total, used uint64
		if n := len(samples); n > 0 {
			total, used = samples[n-1].Total, samples[n-1].Used
		}
		var free uint64
		if total > used {
			free = total - used
		}
		f := forecastFromSamples(samples, total, free, nowTs)
		if samples == nil {
			samples = []store.DestSample{}
		}
		outDests = append(outDests, insightsDest{
			ID: id, Name: name, Type: typ,
			TotalBytes: total, UsedBytes: used,
			DaysToFull: f.DaysToFull, FillDate: f.FillDate, GrowthPerMonth: f.GrowthPerMonth,
			History: samples,
		})
	}
	addDest(localPrimaryID, localPrimaryName, "local")
	for _, d := range dests {
		addDest(d.ID, d.Name, d.Type)
	}

	// --- Restore-drill confidence (latest outcome per backup) ---
	// Index the catalog by backup id so each drill can name the container/stack it
	// tested, instead of showing a bare id. Node names are resolved once and cached.
	byID := make(map[string]*store.Backup, len(list))
	for _, b := range list {
		byID[b.ID] = b
	}
	nodeNames := map[string]string{}
	nodeName := func(id string) string {
		if id == "" {
			return ""
		}
		if n, ok := nodeNames[id]; ok {
			return n
		}
		n := id
		if nd, err := s.store.GetNode(id); err == nil {
			n = nd.Name
		}
		nodeNames[id] = n
		return n
	}

	drills, _ := s.store.ListDrills()
	di := insightsDrills{Recent: []insightsDrill{}}
	for _, dr := range drills {
		di.Total++
		if dr.OK {
			di.Passed++
		} else {
			di.Failed++
		}
	}
	sort.Slice(drills, func(i, j int) bool { return drills[i].RanAt > drills[j].RanAt })
	for i, dr := range drills {
		if i >= 8 {
			break
		}
		rec := insightsDrill{BackupID: dr.BackupID, OK: dr.OK, RanAt: dr.RanAt, Detail: dr.Detail}
		if b, ok := byID[dr.BackupID]; ok {
			rec.Target = b.TargetName
			rec.Stack = b.Stack
			rec.NodeName = nodeName(b.NodeID)
		}
		di.Recent = append(di.Recent, rec)
	}

	// --- RPO adherence for critical databases ---
	ri := insightsRPO{Containers: []insightsRPOItem{}}
	for _, c := range s.loadCriticalDBs() {
		if c.RPOSeconds <= 0 {
			continue
		}
		ri.Total++
		nodeName := c.NodeID
		if nd, err := s.store.GetNode(c.NodeID); err == nil {
			nodeName = nd.Name
		}
		at, ok := s.newestVerifiedFor(c.NodeID, c.Name)
		measured := int64(-1)
		met := false
		if ok {
			measured = nowTs - at
			met = measured <= int64(c.RPOSeconds)
		}
		if met {
			ri.Meeting++
		} else {
			ri.Breaching++
		}
		ri.Containers = append(ri.Containers, insightsRPOItem{
			Node: nodeName, Container: c.Name, TargetSeconds: c.RPOSeconds, MeasuredSeconds: measured, Met: met,
		})
	}
	sort.Slice(ri.Containers, func(i, j int) bool {
		if ri.Containers[i].Met != ri.Containers[j].Met {
			return !ri.Containers[i].Met // breaching first
		}
		return ri.Containers[i].Container < ri.Containers[j].Container
	})

	// --- Per-container backup-size growth (F11): largest & fastest-growing ---
	topGrowth := topGrowthFromBackups(list, nodeName, 8)

	writeJSON(w, http.StatusOK, insightsResp{
		Fleet:        insightsFleet{SuccessRate: rate, Backups30d: t30, BackupsVerified: v30},
		BackupsDaily: daily,
		Destinations: outDests,
		Drills:       di,
		RPO:          ri,
		TopGrowth:    topGrowth,
	})
}

// handleContainerSizes returns a container's successful-backup archive sizes over
// time plus a linear growth rate, for the ContainerDetail size sparkline (F11).
// Read-only, from the backup catalog (no Docker call beyond resolving the name).
func (s *Server) handleContainerSizes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	if _, err := s.store.GetNode(id); err != nil {
		errJSON(w, http.StatusNotFound, "node not found")
		return
	}
	name, err := s.resolveContainerName(id, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	list, _ := s.store.ListBackups(id, 5000)
	pts := []sizePoint{}
	for _, b := range list {
		if b.Status == "success" && b.TargetName == name {
			pts = append(pts, sizePoint{Ts: b.CreatedAt, Bytes: b.SizeBytes})
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].Ts < pts[j].Ts })
	var latest int64
	if len(pts) > 0 {
		latest = pts[len(pts)-1].Bytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "points": pts,
		"growth_bytes_per_month": linearGrowthPerMonth(pts),
		"latest_bytes":           latest,
	})
}
