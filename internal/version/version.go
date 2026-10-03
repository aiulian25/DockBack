// Package version exposes the application version. The value is injected at
// build time via -ldflags "-X dockback/internal/version.Version=<v>" (see the
// Dockerfile). It defaults to "dev" for local/unstamped builds so the UI never
// shows a hardcoded number.
package version

// Version is the running app version (e.g. "v1.0.0"); "dev" when not stamped.
var Version = "dev"
