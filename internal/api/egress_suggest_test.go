package api

import (
	"strings"
	"testing"

	"dockback/internal/dockercli"
	"dockback/internal/notify"
	"dockback/internal/store"
)

// TestEgressSuggestExtraction covers the F54 acceptance scenario: an SMB
// destination, a Gotify URL, and an SSH node yield exactly their three bare
// hostnames with the right source labels — and no password or URL path ever
// appears in the output.
func TestEgressSuggestExtraction(t *testing.T) {
	dests := []destEndpoint{
		{label: "destination: NAS", typ: "smb", cfg: map[string]string{
			"host": "nas.lan", "user": "backup", "password": "s3cr3t-do-not-leak",
		}},
	}
	ncfg := notify.Config{}
	ncfg.Gotify.URL = "https://push.example.com/message?token=abc123-secret"
	ncfg.Gotify.Token = "abc123-secret"
	nodes := []*store.Node{
		{Name: "edge", Transport: dockercli.TransportSSH, Address: "ssh://root@10.0.0.5:22"},
		{Name: "here", Transport: dockercli.TransportLocalProxy, Address: "unix:///var/run/docker.sock"}, // excluded
	}

	got := collectEgressHosts(dests, ncfg, nodes)

	wantHosts := map[string]string{
		"nas.lan":          "destination: NAS",
		"push.example.com": "notification: Gotify",
		"10.0.0.5":         "node: edge",
	}
	if len(got) != len(wantHosts) {
		t.Fatalf("got %d suggestions, want %d: %+v", len(got), len(wantHosts), got)
	}
	// Sorted ascending: 10.0.0.5, nas.lan, push.example.com.
	if got[0].Host != "10.0.0.5" || got[1].Host != "nas.lan" || got[2].Host != "push.example.com" {
		t.Fatalf("hosts not sorted/expected: %+v", got)
	}
	for _, s := range got {
		if wantHosts[s.Host] != s.Source {
			t.Errorf("host %q: source=%q, want %q", s.Host, s.Source, wantHosts[s.Host])
		}
	}

	// SECURITY: no credential or URL path/token may appear anywhere in the payload.
	for _, s := range got {
		blob := s.Host + "|" + s.Source
		for _, leak := range []string{"s3cr3t", "password", "token", "abc123", "/message", "root@", ":22", "unix://"} {
			if strings.Contains(blob, leak) {
				t.Errorf("suggestion leaks %q: %q", leak, blob)
			}
		}
	}
}

// TestEgressSuggestDedupAndWebDAVHost confirms WebDAV/S3 URLs reduce to a bare
// host (no path) and that a host configured twice appears once (first source).
func TestEgressSuggestDedupAndWebDAVHost(t *testing.T) {
	dests := []destEndpoint{
		{label: "destination: Nextcloud", typ: "webdav", cfg: map[string]string{"url": "https://cloud.example.org/remote.php/dav/files/me"}},
		{label: "destination: Dup", typ: "s3", cfg: map[string]string{"endpoint": "https://cloud.example.org:443"}},
	}
	got := collectEgressHosts(dests, notify.Config{}, nil)
	if len(got) != 1 {
		t.Fatalf("expected dedup to 1 host, got %+v", got)
	}
	if got[0].Host != "cloud.example.org" {
		t.Errorf("webdav host = %q, want cloud.example.org (no path)", got[0].Host)
	}
	if got[0].Source != "destination: Nextcloud" {
		t.Errorf("dedup should keep first source, got %q", got[0].Source)
	}
}
