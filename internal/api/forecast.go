package api

import (
	"time"

	"dockback/internal/store"
)

// Storage-capacity forecasting (PLAN §9.13). From a destination's recorded
// capacity history we fit a simple linear trend of USED bytes over time and,
// given the current free space, project the date it fills up. Trending measured
// usage naturally accounts for retention (pruning shows up as the curve
// flattening or dropping), so no separate retention model is needed.

const (
	forecastWindow   = 90 * 24 * time.Hour // history considered for the trend
	forecastMinSpan  = 2 * 24 * 3600       // need ≥2 days between first/last point
	forecastMinCount = 2                   // …and ≥2 points
)

// Forecast is a destination's projected capacity outlook.
type Forecast struct {
	GrowthPerMonth int64 `json:"growth_bytes_per_month"` // signed: negative = shrinking
	FillDate       int64 `json:"fill_date"`              // unix secs, 0 = not projected
	DaysToFull     int64 `json:"days_to_full"`           // -1 = n/a (no total, or not filling)
	Points         int   `json:"history_points"`
}

// forecastFromSamples fits used-bytes vs time and projects fill-up using the
// supplied current total/free (the freshest reading the caller has). Pure.
func forecastFromSamples(samples []store.DestSample, total, free uint64, now int64) Forecast {
	f := Forecast{DaysToFull: -1, Points: len(samples)}
	if len(samples) < forecastMinCount {
		return f
	}
	span := samples[len(samples)-1].Ts - samples[0].Ts
	if span < forecastMinSpan {
		return f
	}
	// Least-squares slope of used (y) over time (x, seconds), mean-centered.
	var n, sx, sy, sxx, sxy float64
	x0 := float64(samples[0].Ts)
	for _, s := range samples {
		x := float64(s.Ts) - x0
		y := float64(s.Used)
		n++
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return f
	}
	slope := (n*sxy - sx*sy) / denom // bytes per second
	f.GrowthPerMonth = int64(slope * 30 * 24 * 3600)
	// Project fill-up only when growing and a real quota is known.
	if slope > 0 && total > 0 && free > 0 {
		secsToFull := float64(free) / slope
		f.DaysToFull = int64(secsToFull / 86400)
		f.FillDate = now + int64(secsToFull)
	}
	return f
}

// destForecast computes a destination's forecast from stored history + the
// supplied current total/free. Cheap (DB read only, no network probe) so it's
// safe to call from the metrics endpoint and the destinations list.
func (s *Server) destForecast(destID string, total, free uint64) Forecast {
	now := time.Now().Unix()
	samples, err := s.store.DestSamples(destID, now-int64(forecastWindow/time.Second))
	if err != nil {
		return Forecast{DaysToFull: -1}
	}
	// If the caller didn't supply a live reading, fall back to the newest sample.
	if total == 0 && free == 0 && len(samples) > 0 {
		last := samples[len(samples)-1]
		total = last.Total
		if last.Total > last.Used {
			free = last.Total - last.Used
		}
	}
	return forecastFromSamples(samples, total, free, now)
}
