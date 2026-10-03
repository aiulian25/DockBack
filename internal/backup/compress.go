package backup

import (
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Compression algorithms recorded in the manifest so restore can pick the right
// decompressor (PLAN §4.15). Empty is treated as zstd for backward compatibility.
const (
	algoZstd = "zstd"
	algoGzip = "gzip"
	algoXz   = "xz"
)

// longWindow is the zstd long-range match window for the "max-long" tier (PLAN
// §9.8). A larger window lets the matcher find repeats far back in the stream
// (long-distance matching) — shrinking large archives with internal redundancy
// (repeated/near-identical files, DB dumps, logs) at a higher CPU/RAM cost. The
// long tier runs single-threaded so peak memory stays ~the window even with
// several concurrent backups under the app's mem_limit.
const longWindow = 32 << 20 // 32 MiB (power of two, required by the zstd encoder)

// decoderMaxWindow caps the window the decoder will allocate. It must be >= any
// encoder window we ever write, so long-range archives decode on verify/restore/
// scrub. Raising it is backward-compatible: small-window frames are unaffected
// and memory is allocated lazily up to the frame's actual window.
const decoderMaxWindow = 64 << 20 // 64 MiB

// parseCompression maps a UI compression choice to the algorithm, the zstd
// encoder level (when applicable), an optional long-range match window (0 =
// library default), and a human label for the manifest. zstd is the default for
// the best speed/ratio balance (PLAN §8.3); gzip is for universal compatibility;
// xz for the absolute maximum ratio (slow, 1 thread); max-long adds long-range
// matching for big, internally-redundant payloads (PLAN §9.8).
func parseCompression(opt string) (algo string, zlevel zstd.EncoderLevel, window int, label string) {
	switch opt {
	case "fast":
		return algoZstd, zstd.SpeedFastest, 0, "zstd-fast"
	case "max":
		return algoZstd, zstd.SpeedBestCompression, 0, "zstd-max"
	case "max-long":
		return algoZstd, zstd.SpeedBestCompression, longWindow, "zstd-max-long"
	case "gzip":
		return algoGzip, 0, 0, "gzip"
	case "xz":
		return algoXz, 0, 0, "xz-max"
	default: // "balanced" and unset
		return algoZstd, zstd.SpeedBetterCompression, 0, "zstd-balanced"
	}
}

// ValidCompression reports whether opt is a compression choice the engine
// recognizes (F79). "" is valid — it means "no per-run override" for callers
// that overlay a run-wide choice on each service's remembered setting. Kept
// next to parseCompression so the two sets can't drift: anything accepted here
// maps to a distinct parseCompression case (never its permissive default).
func ValidCompression(opt string) bool {
	switch opt {
	case "", "fast", "balanced", "max", "max-long", "gzip", "xz":
		return true
	}
	return false
}

// newCompressWriter returns a compressing WriteCloser for the algorithm. The
// caller must Close it to flush the trailer. A non-zero window enables zstd
// long-range matching at that window size (PLAN §9.8); it is ignored by gzip/xz.
func newCompressWriter(w io.Writer, algo string, zlevel zstd.EncoderLevel, window int) (io.WriteCloser, error) {
	switch algo {
	case algoGzip:
		return gzip.NewWriterLevel(w, gzip.BestCompression)
	case algoXz:
		return xz.NewWriter(w)
	default: // zstd
		opts := []zstd.EOption{zstd.WithEncoderLevel(zlevel)}
		if window > 0 {
			// Single-threaded so peak memory is bounded by the window (a multi-
			// threaded encoder holds a window per worker), keeping several concurrent
			// long backups within mem_limit.
			opts = append(opts, zstd.WithWindowSize(window), zstd.WithEncoderConcurrency(1))
		}
		return zstd.NewWriter(w, opts...)
	}
}

// newDecompressReader returns a ReadCloser that decompresses the stream written
// by newCompressWriter for the recorded algorithm (PLAN §4.15). An empty
// algorithm means zstd (pre-§4.15 backups).
func newDecompressReader(r io.Reader, algo string) (io.ReadCloser, error) {
	switch algo {
	case algoGzip:
		return gzip.NewReader(r)
	case algoXz:
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(xr), nil
	case "", algoZstd:
		// Allow the larger window written by the max-long tier (PLAN §9.8).
		d, err := zstd.NewReader(r, zstd.WithDecoderMaxWindow(decoderMaxWindow))
		if err != nil {
			return nil, err
		}
		// zstd.Decoder.Close() returns no error; wrap to satisfy io.ReadCloser.
		return readCloser{Reader: d, closeFn: func() error { d.Close(); return nil }}, nil
	}
	return nil, fmt.Errorf("unknown compression algorithm %q", algo)
}

type readCloser struct {
	io.Reader
	closeFn func() error
}

func (rc readCloser) Close() error {
	if rc.closeFn != nil {
		return rc.closeFn()
	}
	return nil
}
