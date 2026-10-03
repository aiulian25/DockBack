package backup

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// A restore chooses between writing into the container that is there and
// REBUILDING it from the backup's saved configuration. That choice used to be
// made on "did the inspect call return an error", so a Docker API timeout or a
// blip from the socket proxy tore down a container that was there all along and
// recreated it. An unanswered question is not an absent container.
func TestOnlyNotFoundMeansTheContainerIsGone(t *testing.T) {
	// What the SDK actually returns for a missing container: a 404 mapped to its
	// not-found type. This is the ONE error that may take the rebuild path.
	gone := errdefs.NotFound(errors.New("Error response from daemon: No such container: web"))
	if !client.IsErrNotFound(gone) {
		t.Fatal("a 404 from the daemon must be recognised as a missing container")
	}
	// And it survives wrapping, so a helper that adds context does not hide it.
	if !client.IsErrNotFound(fmt.Errorf("inspect web: %w", gone)) {
		t.Error("wrapping must not hide a not-found")
	}

	// Everything else is a question that was not answered, and must NOT rebuild.
	for name, err := range map[string]error{
		"api timeout":        errors.New("context deadline exceeded"),
		"connection refused": errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"),
		"proxy 502":          errdefs.Unavailable(errors.New("bad gateway")),
		"permission denied":  errdefs.Forbidden(errors.New("request blocked by the socket proxy")),
		"daemon error":       errdefs.System(errors.New("internal server error")),
		"cancelled":          errors.New("context canceled"),
	} {
		if client.IsErrNotFound(err) {
			t.Errorf("%s must not be read as a missing container — rebuilding on it destroys a live container", name)
		}
	}

	// A blocked endpoint answering 404 is the one shape that could still fool
	// this; it is the same answer the daemon gives for a missing container, so
	// it is recorded here rather than claimed to be handled.
	blocked := errdefs.NotFound(fmt.Errorf("proxy returned %d", http.StatusNotFound))
	if !client.IsErrNotFound(blocked) {
		t.Skip("SDK no longer maps this shape; revisit the proxy note in restore.go")
	}
}
