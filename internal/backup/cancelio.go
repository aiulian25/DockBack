package backup

import (
	"context"
	"io"
)

// Cancelable streaming.
//
// A backup's long phases are plain io.Copy loops over pipes and sidecar streams
// that take no context: spooling a volume tar, packing + compressing it, and
// encrypting it into storage. Without a context check in the loop, a canceled
// backup keeps running to completion — on a large container that reads as
// "cancel does nothing". ctxReader makes those copies abort at the next chunk.
//
// It complements (not replaces) killing the producer: a sidecar stream is also
// killed at the source (dockercli.abortSidecarOnCancel) so nothing keeps
// producing bytes; this stops the consumer promptly even when the producer is
// local (a file on disk, an in-process pipe).

// ctxReader wraps r so reads fail as soon as ctx is done. It never blocks: the
// check happens before each Read, so cancellation is observed within one chunk
// (a few hundred KB), not at end-of-stream.
func ctxReader(ctx context.Context, r io.Reader) io.Reader {
	if ctx == nil || ctx.Done() == nil {
		return r
	}
	return &cancelReader{r: r, ctx: ctx}
}

type cancelReader struct {
	r   io.Reader
	ctx context.Context
}

func (c *cancelReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
