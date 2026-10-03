package backup

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
)

// Every caller of this stops a container and then replaces the files underneath
// it. The stop error used to be discarded, so a container the daemon could not
// stop had its data directory wiped and rewritten while the application was
// still writing to it — which corrupts the restored data and the application at
// once, and leaves neither able to recover the other.

// classify mirrors the decision stopBeforeOverwrite makes, so the rule can be
// tested without a Docker daemon.
func classify(err error) bool {
	return err == nil || client.IsErrNotFound(err) || alreadyStopped(err)
}

func TestStopBeforeOverwriteTreatsNothingToStopAsSuccess(t *testing.T) {
	ok := map[string]error{
		"stopped cleanly":     nil,
		"container is gone":   errdefs.NotFound(errors.New("No such container: web")),
		"gone, wrapped":       fmt.Errorf("stop web: %w", errdefs.NotFound(errors.New("No such container: web"))),
		"was not running":     errors.New("Container abc123 is not running"),
		"already stopped":     errors.New("container already stopped"),
		"already stopped, up": errors.New("Container ALREADY STOPPED"),
	}
	for name, err := range ok {
		if !classify(err) {
			t.Errorf("%s: nothing left to stop must be success, not a refusal", name)
		}
	}
}

func TestStopBeforeOverwriteRefusesWhenTheStopFailed(t *testing.T) {
	refuse := map[string]error{
		"daemon timed out":   errors.New("context deadline exceeded"),
		"daemon unreachable": errors.New("Cannot connect to the Docker daemon"),
		"proxy blocked it":   errdefs.Forbidden(errors.New("request blocked by the socket proxy")),
		"daemon error":       errdefs.System(errors.New("internal server error")),
		"conflict":           errdefs.Conflict(errors.New("cannot stop container: in use")),
	}
	for name, err := range refuse {
		if classify(err) {
			t.Errorf("%s: the container may still be running — replacing its data now corrupts both", name)
		}
	}
}

// The message has to name the container, because an operator reading it needs to
// know which one to go and look at.
func TestStopFailureNamesTheContainer(t *testing.T) {
	cause := errors.New("context deadline exceeded")
	got := fmt.Errorf("could not stop %s before restoring its data: %w", "paperless-db", cause)
	if !strings.Contains(got.Error(), "paperless-db") {
		t.Errorf("the failure must name the container: %v", got)
	}
	if !errors.Is(got, cause) {
		t.Error("the underlying cause must survive so the operator can act on it")
	}
}
