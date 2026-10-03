package api

import (
	"encoding/json"
	"strconv"
	"testing"

	"dockback/internal/backup"
)

// TestPITRReadinessSingleSourceOfTruth (F49) locks in that the critical-DB card
// and the F34 PITR chip derive their verdict from the SAME parser
// (backup.ParsePGReadiness) — so archive_mode is always part of the definition and
// the two payloads can never disagree again. It mirrors exactly what
// handleGetCritical copies out of the probe after the change.
//
// Regression: before F49 the critical card used `wal_level in (replica,logical) &&
// max_wal_senders > 0` and IGNORED archive_mode, so `replica|off|10` showed
// pitr_ready:true there while the chip showed "Full-dump only".
func TestPITRReadinessSingleSourceOfTruth(t *testing.T) {
	cases := []struct {
		probe       string
		wantReady   bool
		wantWAL     string
		wantArchive string
		wantSenders int
	}{
		// WAL streaming is on, but with NO archive there is no PITR — full dumps only.
		{"replica|off|10", false, "replica", "off", 10},
		// replica + archive_mode=on -> point-in-time recovery is possible.
		{"replica|on|10", true, "replica", "on", 10},
		{"logical|on|5", true, "logical", "on", 5},
	}
	for _, tc := range cases {
		rp := backup.ParsePGReadiness(tc.probe)
		// Exactly the copy handleGetCritical performs into criticalStatus.
		gotReady := rp.Ready
		gotWAL, gotArchive := rp.WALLevel, rp.ArchiveMode
		gotSenders, _ := strconv.Atoi(rp.MaxWALSenders)

		if gotReady != tc.wantReady {
			t.Errorf("%s: pitr_ready=%v, want %v (archive_mode must gate readiness)", tc.probe, gotReady, tc.wantReady)
		}
		if gotWAL != tc.wantWAL || gotArchive != tc.wantArchive {
			t.Errorf("%s: wal_level=%q archive_mode=%q, want %q/%q", tc.probe, gotWAL, gotArchive, tc.wantWAL, tc.wantArchive)
		}
		if gotSenders != tc.wantSenders {
			t.Errorf("%s: max_wal_senders=%d, want %d", tc.probe, gotSenders, tc.wantSenders)
		}
	}

	// A blank/garbled probe leaves wal_level empty, so handleGetCritical never sets
	// PITRChecked (the card shows "not checked", not a false verdict).
	if rp := backup.ParsePGReadiness(""); rp.WALLevel != "" {
		t.Errorf("empty probe: wal_level=%q, want empty (so PITRChecked stays false)", rp.WALLevel)
	}
}

// The PITR panel used to be fed by a probe that handleContainerDetail ran on
// EVERY poll — a docker exec into a production database ten times a minute for a
// read-only panel. That probe is gone and the panel now reads the one this
// endpoint already runs, so everything the panel renders has to survive in this
// payload. The guidance line is the part that would silently disappear: it is
// the sentence telling the operator what to put in postgresql.conf.
func TestCriticalStatusCarriesEverythingThePITRPanelRenders(t *testing.T) {
	for _, probe := range []string{"replica|on|10", "replica|off|10"} {
		rp := backup.ParsePGReadiness(probe)
		st := criticalStatus{}
		// Exactly the copy handleGetCritical performs.
		st.PITRChecked = true
		st.WALLevel, st.ArchiveMode = rp.WALLevel, rp.ArchiveMode
		st.MaxWALSenders, _ = strconv.Atoi(rp.MaxWALSenders)
		st.PITRReady = rp.Ready
		st.PITRDetail = rp.Detail

		raw, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		// The exact field names ContainerBackup.tsx reads.
		for _, field := range []string{"pitr_checked", "pitr_ready", "pitr_detail", "wal_level", "archive_mode", "max_wal_senders"} {
			if _, ok := out[field]; !ok {
				t.Errorf("%s: the panel reads %q and it is not in the payload: %s", probe, field, raw)
			}
		}
		if out["pitr_detail"] == "" {
			t.Errorf("%s: the remediation line must travel with the reading, not be re-derived in the browser", probe)
		}
		if out["pitr_ready"] != rp.Ready {
			t.Errorf("%s: pitr_ready=%v, want %v", probe, out["pitr_ready"], rp.Ready)
		}
	}

	// Nothing checked: the panel's own gate is pitr_checked, and an unset detail
	// must not appear at all rather than showing an empty line.
	raw, _ := json.Marshal(criticalStatus{})
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["pitr_checked"] != false {
		t.Error("an unprobed container must report pitr_checked false")
	}
	if _, present := out["pitr_detail"]; present {
		t.Error("an unset guidance line must be omitted, not rendered empty")
	}
}
