package api

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Per-container Redis password (F205).
//
// A Redis whose password was set at runtime with `CONFIG SET requirepass` hides
// it from every place the dump knows how to look — the environment, the _FILE
// variants, REDIS_ARGS, a redis.conf, the server's own command line. So that
// broker fell back to a raw file capture on EVERY run, losing point-in-time
// consistency each time, while the log advised setting REDIS_PASSWORD, which is
// not how the operator configured it. This is where they record the answer once.
//
// WRITE-ONLY OVER THE API
//
// The value goes in and never comes back out. It is sealed under the master key
// at rest (engine.SetRedisAuth) and the container detail endpoint reports only
// `redis_auth_set` — a boolean. There is no read endpoint, deliberately: this is
// a live credential to a running service, and an API that can hand it back is an
// API that hands it to whoever reaches the API. An operator who has forgotten
// the value re-enters it, which costs them one paste.
//
// It is also never audited by value. The audit trail is readable by any admin
// and is exported inside app backups, so the row records only that a password
// was set or cleared for a named container.

// handleSetRedisAuth records or clears the password for one container's Redis.
//
// Auth + CSRF gated and audited like every other per-container backup option.
// Accepted for ANY container rather than only ones currently detected as Redis:
// detection reads a running container, and an operator recording the password
// for a broker that is stopped, or about to be recreated, is doing something
// reasonable. A setting on a container that never dumps Redis simply never
// applies — it costs nothing and refusing it would be the more surprising
// behaviour.
func (s *Server) handleSetRedisAuth(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cid := r.PathValue("cid")
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid request")
		return
	}
	cli, err := s.reg.Get(id)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	insp, err := cli.ContainerInspect(ctx, cid)
	if err != nil {
		errJSON(w, http.StatusBadGateway, err.Error())
		return
	}
	name := strings.TrimPrefix(insp.Name, "/")

	// Trailing whitespace from a copy-paste would produce a password that is
	// wrong in a way nothing can show you — the field is masked, so the stray
	// space is invisible and the only symptom is a broker that keeps failing to
	// authenticate. Trimmed once, here, rather than at the point of use.
	pw := strings.TrimSpace(body.Password)
	if err := s.engine.SetRedisAuth(id, name, pw); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	action := "redis.auth.set"
	if pw == "" {
		action = "redis.auth.cleared"
	}
	_ = s.store.Audit(userFrom(r), action, name, "per-container Redis password (value not recorded)")
	writeJSON(w, http.StatusOK, map[string]any{"redis_auth_set": s.engine.HasRedisAuth(id, name)})
}
