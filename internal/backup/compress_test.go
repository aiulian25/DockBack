package backup

import (
	"bytes"
	"io"
	"testing"
)

func TestParseCompression(t *testing.T) {
	cases := map[string]string{"fast": algoZstd, "balanced": algoZstd, "": algoZstd, "max": algoZstd, "max-long": algoZstd, "gzip": algoGzip, "xz": algoXz}
	for opt, wantAlgo := range cases {
		if got, _, _, _ := parseCompression(opt); got != wantAlgo {
			t.Errorf("parseCompression(%q) algo = %q, want %q", opt, got, wantAlgo)
		}
	}
	// Only max-long enables a long-range window; everything else uses the default.
	if _, _, win, _ := parseCompression("max-long"); win != longWindow {
		t.Errorf("max-long window = %d, want %d", win, longWindow)
	}
	for _, opt := range []string{"fast", "balanced", "max", "gzip", "xz", ""} {
		if _, _, win, _ := parseCompression(opt); win != 0 {
			t.Errorf("%q window = %d, want 0", opt, win)
		}
	}
}

func TestCompressRoundTripAllAlgorithms(t *testing.T) {
	payload := bytes.Repeat([]byte("dockback compression payload — "), 4000)
	for _, opt := range []string{"balanced", "fast", "max", "max-long", "gzip", "xz"} {
		algo, zlevel, window, _ := parseCompression(opt)

		var compressed bytes.Buffer
		w, err := newCompressWriter(&compressed, algo, zlevel, window)
		if err != nil {
			t.Fatalf("%s: writer: %v", opt, err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatalf("%s: write: %v", opt, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("%s: close: %v", opt, err)
		}
		if compressed.Len() >= len(payload) {
			t.Errorf("%s: did not compress (%d >= %d)", opt, compressed.Len(), len(payload))
		}

		// Decompress via the recorded algorithm — this is the restore path.
		r, err := newDecompressReader(bytes.NewReader(compressed.Bytes()), algo)
		if err != nil {
			t.Fatalf("%s: reader: %v", opt, err)
		}
		out, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatalf("%s: read: %v", opt, err)
		}
		if !bytes.Equal(out, payload) {
			t.Fatalf("%s: round-trip mismatch", opt)
		}
	}

	// Backward compatibility: an empty algorithm decompresses as zstd.
	algo, zlevel, window, _ := parseCompression("balanced")
	var c bytes.Buffer
	w, _ := newCompressWriter(&c, algo, zlevel, window)
	_, _ = w.Write(payload)
	_ = w.Close()
	r, err := newDecompressReader(bytes.NewReader(c.Bytes()), "") // legacy: no algorithm field
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(out, payload) {
		t.Fatal("empty algorithm must decompress as zstd (back-compat)")
	}
}
