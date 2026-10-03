package dockercli

import (
	"context"
	"io"
	"time"

	"github.com/docker/docker/client"
)

// Cancelable sidecar streams.
//
// A volume-capture sidecar streams its tar through an attached connection into
// an io.Pipe. Neither the attach nor the copy takes a context, so once the
// stream is running a canceled backup used to keep going until the sidecar had
// tarred the ENTIRE volume — on a large container, effectively "cancel does
// nothing". abortSidecarOnCancel closes that gap: when ctx is canceled it kills
// the sidecar (so tar stops producing immediately) and closes the pipe, which
// unblocks both the reader and the streaming goroutine. The goroutine's normal
// cleanup path then removes the container as usual, so a cancel leaves no stray
// sidecar behind.
//
// Callers close `done` when the stream finishes normally, so the watcher exits
// without touching anything.
func abortSidecarOnCancel(ctx context.Context, c *client.Client, sidecarID string, pw *io.PipeWriter, done <-chan struct{}) {
	if ctx == nil || ctx.Done() == nil {
		return // a background context can never cancel — no watcher needed
	}
	go func() {
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		// Kill (not stop) — this is a throwaway read-only sidecar with nothing to
		// flush, and SIGKILL ends a multi-GB tar instantly instead of waiting out
		// a graceful-stop timeout.
		kctx, kcancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = c.ContainerKill(kctx, sidecarID, "KILL")
		kcancel()
		// Unblock whoever is reading the tar, with the reason.
		pw.CloseWithError(ctx.Err())
	}()
}
