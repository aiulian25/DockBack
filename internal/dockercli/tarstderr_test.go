package dockercli

import (
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/pkg/stdcopy"
)

// A restore that fails because the disk filled, or because a bind is read-only,
// used to be reported as "restore tar exited 2". The operator then had to
// reproduce the failure by hand to find out which of the two it was — during a
// restore, which is the worst possible moment to be guessing.

func TestTarFailureDetailQuotesWhatTarSaid(t *testing.T) {
	cases := map[string]struct{ stderr, want string }{
		"a full disk": {
			"tar: /var/lib/data/big.bin: Cannot write: No space left on device\n",
			": tar: /var/lib/data/big.bin: Cannot write: No space left on device",
		},
		"a read-only bind": {
			"tar: /etc/app/config.yml: Cannot open: Permission denied\n",
			": tar: /etc/app/config.yml: Cannot open: Permission denied",
		},
		"several complaints become one line": {
			"tar: a: Permission denied\ntar: b: Permission denied\n",
			": tar: a: Permission denied; tar: b: Permission denied",
		},
		"silence stays silent":  {"", ""},
		"whitespace is silence": {"  \n\n ", ""},
	}
	for what, c := range cases {
		if got := tarFailureDetail(c.stderr); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", what, got, c.want)
		}
	}
}

// The drain is the mechanism: tar's stderr arrives multiplexed on the attach
// stream, and it has to be read WHILE the tar is being uploaded — an undrained
// pipe blocks tar as soon as it fills.
func TestTarStderrIsCapturedFromTheMultiplexedStream(t *testing.T) {
	var frames strings.Builder
	w := stdcopy.NewStdWriter(&frames, stdcopy.Stderr)
	if _, err := w.Write([]byte("tar: /data/x: Cannot write: No space left on device\n")); err != nil {
		t.Fatal(err)
	}
	// tar also writes ordinary output; only stderr belongs in the failure.
	out := stdcopy.NewStdWriter(&frames, stdcopy.Stdout)
	if _, err := out.Write([]byte("extracting ./data/x\n")); err != nil {
		t.Fatal(err)
	}

	stderr := &tailWriter{limit: maxTarStderrTail}
	if _, err := stdcopy.StdCopy(io.Discard, stderr, strings.NewReader(frames.String())); err != nil {
		t.Fatal(err)
	}

	detail := tarFailureDetail(stderr.String())
	if !strings.Contains(detail, "No space left on device") {
		t.Errorf("the cause must survive to the operator: %q", detail)
	}
	if strings.Contains(detail, "extracting") {
		t.Errorf("ordinary output is not a failure reason: %q", detail)
	}

	// The whole message a caller would see.
	full := "restore tar exited 2" + detail
	if !strings.Contains(full, "exited 2") || !strings.Contains(full, "No space left") {
		t.Errorf("the error must carry both the code and the reason: %q", full)
	}
}

// A pathological archive can make tar print a line per member. The capture is
// bounded, and keeps the END, where tar reports the failure that stopped it.
func TestTarStderrIsBounded(t *testing.T) {
	stderr := &tailWriter{limit: maxTarStderrTail}
	for i := 0; i < 5000; i++ {
		if _, err := stderr.Write([]byte("tar: some/long/path/that/repeats: Cannot mkdir: File exists\n")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stderr.Write([]byte("tar: Exiting with failure status due to previous errors\n")); err != nil {
		t.Fatal(err)
	}
	if len(stderr.buf) > maxTarStderrTail {
		t.Errorf("retained %d bytes, want at most %d", len(stderr.buf), maxTarStderrTail)
	}
	detail := tarFailureDetail(stderr.String())
	if !strings.Contains(detail, "Exiting with failure status") {
		t.Errorf("the last thing tar said is the one that matters: %q", detail)
	}
	if !strings.Contains(detail, "earlier output dropped") {
		t.Errorf("a truncated capture must say so: %q", detail)
	}
}
