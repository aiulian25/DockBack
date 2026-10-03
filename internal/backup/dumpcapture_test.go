package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

// F87 records what a dump DECLARED at capture, so "the stored dump was complete
// when written" becomes a fact on record rather than something re-derived later
// from a copy that may since have been damaged.
//
// The incident behind it: a verified-good Postgres dump restored as 27 of 72
// primary keys and 0 of 115 foreign keys, and was reported a success.

const captureDump = `--
-- PostgreSQL database cluster dump
--
CREATE ROLE app;
CREATE DATABASE app;
\connect app
CREATE TABLE public.doc (id integer NOT NULL);
ALTER TABLE ONLY public.doc
    ADD CONSTRAINT doc_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.tag
    ADD CONSTRAINT tag_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.doc_tag
    ADD CONSTRAINT doc_tag_fk FOREIGN KEY (doc_id) REFERENCES public.doc(id);
--
-- PostgreSQL database cluster dump complete
--
`

// The tally is written THROUGH on the capture path (the dump is being written to
// disk), so it must pass every byte on unchanged while counting.
func TestDumpCaptureTallyPassesBytesThrough(t *testing.T) {
	for _, chunk := range []int{1, 7, 64, 8192} {
		tally := NewDumpTally()
		var got strings.Builder
		w := io.MultiWriter(&got, tally)
		src := captureDump
		for len(src) > 0 {
			n := chunk
			if n > len(src) {
				n = len(src)
			}
			if _, err := w.Write([]byte(src[:n])); err != nil {
				t.Fatalf("chunk %d: %v", chunk, err)
			}
			src = src[n:]
		}
		if got.String() != captureDump {
			t.Fatalf("chunk %d: the dump must reach its destination unaltered", chunk)
		}
		exp := tally.Expect()
		if exp.PrimaryKeys != 2 || exp.ForeignKeys != 1 {
			t.Fatalf("chunk %d: tally = %d PK / %d FK, want 2/1", chunk, exp.PrimaryKeys, exp.ForeignKeys)
		}
		if !exp.Complete || !exp.HeaderSeen {
			t.Fatalf("chunk %d: header/trailer = %v/%v, want both", chunk, exp.HeaderSeen, exp.Complete)
		}
		if exp.Bytes != int64(len(captureDump)) {
			t.Fatalf("chunk %d: bytes = %d, want %d", chunk, exp.Bytes, len(captureDump))
		}
		// The checksum must be of the dump exactly as written.
		want := sha256.Sum256([]byte(captureDump))
		if exp.SHA256 != hex.EncodeToString(want[:]) {
			t.Fatalf("chunk %d: sha256 = %q", chunk, exp.SHA256)
		}
		if len(exp.SHA256) != 64 {
			t.Fatalf("chunk %d: sha256 must be 64 hex chars, got %d", chunk, len(exp.SHA256))
		}
	}
}

// Expect() flushes a final line with no trailing newline and is safe to call
// twice — the engine reads it once, tests read it again.
func TestDumpCaptureExpectIsIdempotent(t *testing.T) {
	tally := NewDumpTally()
	tally.Write([]byte("-- PostgreSQL database dump\nALTER TABLE ONLY x\n    ADD CONSTRAINT p PRIMARY KEY (id);"))
	a := tally.Expect()
	b := tally.Expect()
	if a != b {
		t.Fatalf("Expect must be stable: %+v vs %+v", a, b)
	}
	if a.PrimaryKeys != 1 {
		t.Fatalf("a final line with no newline must still be tallied, got %d", a.PrimaryKeys)
	}
}

// THE capture-time gate. Each of these is a dump that must never be stored.
func TestDumpCaptureTruncationIsDetected(t *testing.T) {
	cases := map[string]struct {
		body string
		want bool
	}{
		"complete dump": {captureDump, false},
		"cut after constraints": {
			captureDump[:strings.Index(captureDump, "--\n-- PostgreSQL database cluster dump complete")], true,
		},
		"cut before any constraint": {
			"--\n-- PostgreSQL database cluster dump\n--\nCREATE ROLE app;\n", true,
		},
		"cut mid-line": {
			"--\n-- PostgreSQL database cluster dump\n--\nCREATE TAB", true,
		},
		// Not a pg_dump at all (a MySQL dump, a binary archive): no header, so no
		// claim is made either way and capture must not fail it.
		"not a postgres dump": {
			"-- MySQL dump 10.13  Distrib 8.0.35\nCREATE TABLE t (id int);\n", false,
		},
		"empty": {"", false},
	}
	for name, c := range cases {
		tally := NewDumpTally()
		tally.Write([]byte(c.body))
		if got := tally.Expect().Truncated(); got != c.want {
			t.Errorf("%s: Truncated() = %v, want %v", name, got, c.want)
		}
	}
}

// A dump cut before its first constraint is the case a constraint-count check
// alone cannot see — it is why the header marker exists.
func TestDumpCaptureEarlyTruncationHasNoConstraintsToCount(t *testing.T) {
	tally := NewDumpTally()
	tally.Write([]byte("--\n-- PostgreSQL database cluster dump\n--\nCREATE ROLE app;\n"))
	exp := tally.Expect()
	if exp.PrimaryKeys != 0 || exp.ForeignKeys != 0 {
		t.Fatal("test premise: this dump has no constraints")
	}
	if !exp.Truncated() {
		t.Fatal("an early truncation must still be caught — that is what HeaderSeen is for")
	}
}

// At restore, the RECORDED contract must win over the re-derived one: if the
// stored dump has been truncated in storage, the streamed tally agrees with the
// damaged copy and would quietly lower the bar.
func TestExpectedForPrefersRecordedContract(t *testing.T) {
	recorded := &DBDump{DumpPrimaryKeys: 72, DumpForeignKeys: 115, DumpComplete: true}
	// What a truncated stored copy would produce on the way into the import.
	streamed := DumpExpect{PrimaryKeys: 27, ForeignKeys: 0, Complete: true, HeaderSeen: true}

	got := expectedFor(recorded, streamed)
	if got.PrimaryKeys != 72 || got.ForeignKeys != 115 {
		t.Fatalf("recorded contract must win: %+v", got)
	}
	// And that contract must then FAIL the restore — the exact incident.
	if err := VerifyImportCompleteness(got, 27, 0); err == nil {
		t.Fatal("a restore landing 27 of 72 primary keys MUST fail")
	}

	// A legacy backup (no recorded numbers) falls back to the streamed tally.
	if got := expectedFor(&DBDump{}, streamed); got.PrimaryKeys != 27 {
		t.Fatalf("legacy backup must fall back to the streamed tally: %+v", got)
	}
	if got := expectedFor(nil, streamed); got.PrimaryKeys != 27 {
		t.Fatalf("a nil record must fall back to the streamed tally: %+v", got)
	}
}

// A read that is itself cut short must be reported even when the manifest says
// the stored dump is complete.
func TestExpectedForCatchesATruncatedRead(t *testing.T) {
	recorded := &DBDump{DumpPrimaryKeys: 72, DumpForeignKeys: 115, DumpComplete: true}
	streamed := DumpExpect{PrimaryKeys: 30, Complete: false, HeaderSeen: true}
	got := expectedFor(recorded, streamed)
	if got.Complete {
		t.Fatal("a read that never saw the trailer must not be reported as complete")
	}
	if err := VerifyImportCompleteness(got, 72, 115); err == nil {
		t.Fatal("a truncated read must fail even when the counts are met")
	}
}

// The manifest record is found by archive-entry path, so the right contract is
// applied when a backup holds several dumps.
func TestDBDumpForEntry(t *testing.T) {
	man := &Manifest{Databases: []DBDump{
		{Service: "pg", Path: "db/pg.sql", DumpPrimaryKeys: 72},
		{Service: "my", Path: "db/my.sql", DumpPrimaryKeys: 5},
	}}
	if d := dbDumpForEntry(man, "db/my.sql"); d == nil || d.DumpPrimaryKeys != 5 {
		t.Fatalf("wrong record for db/my.sql: %+v", d)
	}
	if d := dbDumpForEntry(man, "db/pg.sql"); d == nil || d.DumpPrimaryKeys != 72 {
		t.Fatalf("wrong record for db/pg.sql: %+v", d)
	}
	if d := dbDumpForEntry(man, "db/absent.sql"); d != nil {
		t.Fatalf("an unknown entry must have no record, got %+v", d)
	}
	if d := dbDumpForEntry(nil, "db/pg.sql"); d != nil {
		t.Fatal("a nil manifest must yield no record")
	}
}
