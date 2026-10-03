package backup

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// The scanner must tally what a dump declares WITHOUT altering the stream the
// importer sees, and the verifier must catch a silent shortfall — the failure
// that produced a Paperless database with 27 of 72 primary keys and no foreign
// keys, restored "successfully".

const sampleDump = `--
-- PostgreSQL database cluster dump
--
CREATE ROLE paperlessuser;
CREATE DATABASE paperless;
\connect paperless
CREATE TABLE public.documents_document (id integer NOT NULL, title text);
COPY public.documents_document (id, title) FROM stdin;
1	ADD CONSTRAINT fake PRIMARY KEY (should not count - this is data)
\.
ALTER TABLE ONLY public.documents_document
    ADD CONSTRAINT documents_document_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.documents_workflowtrigger
    ADD CONSTRAINT documents_workflowtrigger_pkey PRIMARY KEY (id);
ALTER TABLE ONLY public.documents_workflow_triggers
    ADD CONSTRAINT documents_workflow_trigg_workflow_id_fk FOREIGN KEY (workflow_id) REFERENCES public.documents_workflow(id);
--
-- PostgreSQL database cluster dump complete
--
`

// readAll through the scanner in awkward chunk sizes: the tally must be
// chunk-boundary-proof, and the bytes must come through byte-for-byte.
func TestDumpScannerTalliesAndPassesThrough(t *testing.T) {
	for _, chunk := range []int{1, 7, 64, 4096} {
		s := newDumpScanner(iotest(strings.NewReader(sampleDump), chunk))
		var got bytes.Buffer
		if _, err := io.Copy(&got, s); err != nil {
			t.Fatalf("chunk %d: %v", chunk, err)
		}
		if got.String() != sampleDump {
			t.Fatalf("chunk %d: the stream must pass through unaltered", chunk)
		}
		exp := s.Expect()
		if exp.PrimaryKeys != 2 {
			t.Fatalf("chunk %d: primary keys = %d, want 2 (the COPY data line must NOT count)", chunk, exp.PrimaryKeys)
		}
		if exp.ForeignKeys != 1 {
			t.Fatalf("chunk %d: foreign keys = %d, want 1", chunk, exp.ForeignKeys)
		}
		if !exp.Complete {
			t.Fatalf("chunk %d: the completion trailer must be detected", chunk)
		}
		if exp.Bytes != int64(len(sampleDump)) {
			t.Fatalf("chunk %d: byte count = %d, want %d", chunk, exp.Bytes, len(sampleDump))
		}
	}
}

// A stream cut before the trailer is exactly the production failure: no errors,
// clean exit, incomplete schema.
func TestDumpScannerDetectsTruncation(t *testing.T) {
	cut := sampleDump[:strings.Index(sampleDump, "ALTER TABLE ONLY public.documents_workflowtrigger")]
	s := newDumpScanner(strings.NewReader(cut))
	_, _ = io.Copy(io.Discard, s)
	exp := s.Expect()
	if exp.Complete {
		t.Fatal("a truncated dump must NOT report complete")
	}
	if exp.PrimaryKeys != 1 {
		t.Fatalf("truncated tally = %d PKs, want 1", exp.PrimaryKeys)
	}
}

func TestVerifyImportCompleteness(t *testing.T) {
	full := DumpExpect{PrimaryKeys: 72, ForeignKeys: 115, Complete: true}

	// The real incident: the dump declared 72/115, the database ended up with
	// 27 and none — and it had been reported as a successful restore.
	err := VerifyImportCompleteness(full, 27, 0)
	if err == nil {
		t.Fatal("a shortfall MUST fail the restore")
	}
	for _, want := range []string{"27 of 72 primary keys", "0 of 115 foreign keys", "INCOMPLETE"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must state the shortfall (%q): %v", want, err)
		}
	}

	// An exact match passes.
	if err := VerifyImportCompleteness(full, 72, 115); err != nil {
		t.Fatalf("a complete restore must pass: %v", err)
	}
	// MORE than declared is fine — other databases in the same cluster count too.
	if err := VerifyImportCompleteness(full, 90, 200); err != nil {
		t.Fatalf("a superset must not fail: %v", err)
	}
	// A dump that declares no keys (data-only) is not checkable — never fail it.
	if err := VerifyImportCompleteness(DumpExpect{Complete: false}, 0, 0); err != nil {
		t.Fatalf("a non-schema dump must not be failed: %v", err)
	}
	// Counts match but the trailer never arrived → still a failure, because the
	// tally itself only covers what we saw before the cut.
	if err := VerifyImportCompleteness(DumpExpect{PrimaryKeys: 3, ForeignKeys: 1}, 3, 1); err == nil {
		t.Fatal("a missing completion marker must fail even when the seen counts match")
	}
}

func TestParsePGConstraintCounts(t *testing.T) {
	// Two databases in the cluster, summed.
	pk, fk := parsePGConstraintCounts("p|72\nf|115\np|3\nf|2\n")
	if pk != 75 || fk != 117 {
		t.Fatalf("sum = %d/%d, want 75/117", pk, fk)
	}
	// Noise, blanks and unexpected types are ignored rather than miscounted.
	if pk, fk := parsePGConstraintCounts("\npsql: warning\nu|59\nc|40\n"); pk != 0 || fk != 0 {
		t.Fatalf("non p/f rows must be ignored, got %d/%d", pk, fk)
	}
}

// iotest returns a reader that yields at most n bytes per Read, to exercise
// chunk boundaries.
func iotest(r io.Reader, n int) io.Reader { return &chunked{r: r, n: n} }

type chunked struct {
	r io.Reader
	n int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}
