package api

import (
	"net/http"
	"sort"
	"strings"

	"dockback/internal/dockercli"
	"dockback/internal/egress"
	"dockback/internal/notify"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// egressSuggestion is one host DockBack is already configured to reach, offered
// for the egress allow-list. Source says where it came from (for a UI chip);
// AllowedNow reports whether the LIVE policy already permits it — so the user sees
// exactly which configured endpoints the current list would still allow, and which
// a typo has locked out. Only a bare hostname is ever emitted (F54).
type egressSuggestion struct {
	Host       string `json:"host"`
	Source     string `json:"source"`
	AllowedNow bool   `json:"allowed_now"`
}

// destEndpoint is one decrypted destination row fed to the pure extractor: its
// display label plus the type + config the storage factory reads its endpoint
// from. Kept minimal so collectEgressHosts is unit-testable without a DB or crypto.
type destEndpoint struct {
	label string
	typ   string
	cfg   map[string]string
}

// collectEgressHosts is the PURE extraction behind the suggestions endpoint — no
// DB, no crypto, no network — so it is unit-tested with fabricated inputs. It maps
// every configured outbound endpoint (destinations, app-destinations, notification
// channels, non-local nodes) to a bare hostname via egress.HostFrom — the same
// normalization the policy applies, which strips any URL path, port, and userinfo
// so no credential ever appears — dedups (first source wins), sorts, and marks each
// against the live default policy.
func collectEgressHosts(dests []destEndpoint, notifyCfg notify.Config, nodes []*store.Node) []egressSuggestion {
	source := map[string]string{} // host -> first source seen
	var order []string
	add := func(raw, src string) {
		h := strings.ToLower(egress.HostFrom(raw))
		if h == "" {
			return
		}
		if _, ok := source[h]; ok {
			return
		}
		source[h] = src
		order = append(order, h)
	}

	for _, d := range dests {
		add(storage.RemoteHost(d.typ, d.cfg), d.label)
	}
	if g := notifyCfg.Gotify; g.URL != "" {
		add(g.URL, "notification: Gotify")
	}
	if e := notifyCfg.Email; e.Host != "" {
		add(e.Host, "notification: email (SMTP)")
	}
	if wh := notifyCfg.Webhook; wh.URL != "" {
		add(wh.URL, "notification: webhook")
	}
	if hb := notifyCfg.Heartbeat; hb.URL != "" {
		add(hb.URL, "notification: heartbeat")
	}
	for _, n := range nodes {
		// Local-proxy nodes reach Docker over the internal socket-proxy, never out
		// to the internet, so they don't belong in an egress allow-list.
		if n == nil || n.Transport == dockercli.TransportLocalProxy || n.Address == "" {
			continue
		}
		add(n.Address, "node: "+n.Name)
	}

	sort.Strings(order)
	out := make([]egressSuggestion, 0, len(order))
	for _, h := range order {
		out = append(out, egressSuggestion{Host: h, Source: source[h], AllowedNow: egress.Default().Check(h) == nil})
	}
	return out
}

// handleEgressSuggestions returns the hosts DockBack is already configured to
// reach, so the user can compose the egress allow-list from real endpoints instead
// of hand-typing (and mistyping) them. Read-only; decrypts destination configs
// only to read their endpoint host and never returns credentials or full URLs.
func (s *Server) handleEgressSuggestions(w http.ResponseWriter, r *http.Request) {
	var dests []destEndpoint
	gather := func(list []*store.Destination, prefix string) {
		for _, d := range list {
			cfg, err := s.decryptDestConfig(d)
			if err != nil {
				continue // a config we can't open contributes no host — skip quietly
			}
			dests = append(dests, destEndpoint{label: prefix + d.Name, typ: d.Type, cfg: cfg})
		}
	}
	if ds, err := s.store.ListDestinations(); err == nil {
		gather(ds, "destination: ")
	}
	if ds, err := s.store.ListAppDestinations(); err == nil {
		gather(ds, "app-backup destination: ")
	}
	notifyCfg, _ := s.loadNotifyConfig()
	nodes, _ := s.store.ListNodes()

	writeJSON(w, http.StatusOK, collectEgressHosts(dests, notifyCfg, nodes))
}
