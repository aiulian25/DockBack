package dockercli

import (
	"strings"
	"testing"
)

// Diagnosing a failed restore used to require SSHing to the host and running
// `docker logs` — the actual cause (an unset PAPERLESS_SECRET_KEY) was only ever
// visible there. These tests pin the two things that make bringing that output
// into DockBack safe: the tail is what survives trimming, and credentials do not
// leave the machine.

// Docker frames a non-TTY stream with an 8-byte header per chunk. Passing that
// through raw would put control bytes in the run log.
func TestDemuxDockerLogStripsFraming(t *testing.T) {
	framed := append(
		append([]byte{1, 0, 0, 0, 0, 0, 0, 6}, []byte("stdout")...),
		append([]byte{2, 0, 0, 0, 0, 0, 0, 6}, []byte("stderr")...)...,
	)
	got := demuxDockerLog(framed)
	if !strings.Contains(got, "stdout") || !strings.Contains(got, "stderr") {
		t.Fatalf("both streams must be demuxed: %q", got)
	}
	if strings.ContainsRune(got, 0) {
		t.Fatalf("framing bytes must not survive: %q", got)
	}
	// stderr last: a crash message is what the reader is looking for.
	if strings.Index(got, "stderr") < strings.Index(got, "stdout") {
		t.Fatalf("stderr should come last: %q", got)
	}
}

// A TTY container's logs have no framing at all; treating them as framed would
// mangle them.
func TestDemuxDockerLogPassesTTYThrough(t *testing.T) {
	raw := "ImproperlyConfigured: PAPERLESS_SECRET_KEY is not set\n"
	if got := demuxDockerLog([]byte(raw)); got != raw {
		t.Fatalf("unframed output must pass through unchanged: %q", got)
	}
	if got := demuxDockerLog(nil); got != "" {
		t.Fatalf("empty input must yield empty output: %q", got)
	}
}

// The cause of a startup failure is in the LAST lines, so trimming must drop the
// beginning — the opposite of a normal truncation.
func TestTrimToTailKeepsTheEnd(t *testing.T) {
	body := strings.Repeat("noise line that is not the problem\n", 500)
	full := body + "FATAL: the actual cause\n"

	got := trimToTail(strings.TrimRight(full, "\n"), 200)
	if !strings.Contains(got, "FATAL: the actual cause") {
		t.Fatal("trimming must keep the END — that is where the error is")
	}
	if len(got) > 260 { // the cap plus the short "trimmed" notice
		t.Fatalf("output must respect the cap, got %d bytes", len(got))
	}
	if !strings.Contains(got, "earlier output trimmed") {
		t.Fatalf("the reader must be told output was dropped: %q", got)
	}
	// Under the cap, nothing is touched or announced.
	short := "one line"
	if got := trimToTail(short, 200); got != short {
		t.Fatalf("short output must be untouched: %q", got)
	}
}

func TestLastLines(t *testing.T) {
	in := "a\nb\nc\nd\ne"
	if got := LastLines(in, 2); got != "d\ne" {
		t.Fatalf("LastLines(2) = %q", got)
	}
	if got := LastLines(in, 99); got != in {
		t.Fatalf("asking for more lines than exist must return all: %q", got)
	}
	if got := LastLines("", 5); got != "" {
		t.Fatalf("empty input must stay empty: %q", got)
	}
	if got := LastLines(in, 0); got != "" {
		t.Fatalf("zero lines must return nothing: %q", got)
	}
}

// The excerpt in an alert leaves the machine — to a Gotify server, an SMTP relay
// or an arbitrary webhook. Container startup output routinely contains
// connection strings.
func TestRedactLogSecretsMasksCredentials(t *testing.T) {
	in := strings.Join([]string{
		"Starting worker",
		"DATABASE_URL=postgres://appuser:sup3rs3cret@db:5432/app",
		"PAPERLESS_SECRET_KEY=abcd1234efgh",
		`API_TOKEN: "tok_live_9999"`,
		"ImproperlyConfigured: PAPERLESS_SECRET_KEY is not set",
		"listening on 0.0.0.0:8000",
	}, "\n")

	got := RedactLogSecrets(in)
	for _, leaked := range []string{"sup3rs3cret", "abcd1234efgh", "tok_live_9999"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("credential %q must not survive redaction:\n%s", leaked, got)
		}
	}
	// The DIAGNOSTIC value must survive — masking the message that explains the
	// failure would defeat the whole feature.
	if !strings.Contains(got, "ImproperlyConfigured: PAPERLESS_SECRET_KEY is not set") {
		t.Fatalf("the actual error line must be preserved:\n%s", got)
	}
	for _, keep := range []string{"Starting worker", "listening on 0.0.0.0:8000", "postgres://appuser:"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("non-secret context must be preserved (%q):\n%s", keep, got)
		}
	}
}

// Redaction must never be destructive on ordinary output, or operators will stop
// trusting the excerpt.
func TestRedactLogSecretsLeavesOrdinaryOutputAlone(t *testing.T) {
	in := "level=info msg=\"ready\" port=8080 db=app user=appuser\nmigrations applied: 42\n"
	if got := RedactLogSecrets(in); got != in {
		t.Fatalf("ordinary output must be unchanged:\nwant %q\ngot  %q", in, got)
	}
	if got := RedactLogSecrets(""); got != "" {
		t.Fatal("empty input must stay empty")
	}
}
