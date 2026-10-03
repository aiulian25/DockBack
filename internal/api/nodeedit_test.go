package api

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"

	"dockback/internal/backup"
	"dockback/internal/config"
	"dockback/internal/dockercli"
	"dockback/internal/storage"
	"dockback/internal/store"
)

// TestResolveEditSecret covers the node-edit credential merge: blank fields keep
// the stored value (per SSH auth method), a new value replaces it, and switching
// SSH auth method (key↔password) clears the other. Fixes the "ssh: no key found"
// Test-Connection bug and backs SSH password auth.
func TestResolveEditSecret(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	s := &Server{cfg: &config.Config{EncryptionKey: key}}
	ssh := dockercli.TransportSSH

	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----"
	keyNode := &store.Node{Transport: ssh, SecretEnc: SealNodeSecret(sshSecretBlob(pem, "hunter2", ""), key)}

	creds := func(b []byte) dockercli.SSHCreds { return dockercli.ParseSSHCreds(b) }

	// Key node, blank edit → keep stored key + passphrase (the reported bug).
	if c := creds(s.resolveEditSecret(ssh, "", "", "", "", keyNode)); string(c.Key) != pem || c.Passphrase != "hunter2" || c.Password != "" {
		t.Fatalf("blank key edit must keep stored key+pass, no password; got %+v", c)
	}
	// New key, blank passphrase → new key + stored passphrase.
	if c := creds(s.resolveEditSecret(ssh, "NEWKEY", "", "", "key", keyNode)); string(c.Key) != "NEWKEY" || c.Passphrase != "hunter2" {
		t.Fatalf("new key/blank pass → NEWKEY+stored pass; got %+v", c)
	}
	// Switch a KEY node to PASSWORD auth → key/passphrase cleared, password set.
	if c := creds(s.resolveEditSecret(ssh, "", "", "s3cret", "password", keyNode)); len(c.Key) != 0 || c.Passphrase != "" || c.Password != "s3cret" {
		t.Fatalf("switch to password must clear key; got %+v", c)
	}

	// Password node: blank password edit keeps the stored password.
	pwNode := &store.Node{Transport: ssh, SecretEnc: SealNodeSecret(sshSecretBlob("", "", "stored-pw"), key)}
	if c := creds(s.resolveEditSecret(ssh, "", "", "", "", pwNode)); c.Password != "stored-pw" || len(c.Key) != 0 {
		t.Fatalf("blank password edit must keep stored password; got %+v", c)
	}
	// Switch a PASSWORD node back to KEY auth → password cleared.
	if c := creds(s.resolveEditSecret(ssh, pem, "", "", "key", pwNode)); string(c.Key) != pem || c.Password != "" {
		t.Fatalf("switch to key must clear password; got %+v", c)
	}

	// Add flow (existing == nil): a submitted password ⇒ password auth.
	if c := creds(s.resolveEditSecret(ssh, "", "", "pw", "", nil)); c.Password != "pw" || len(c.Key) != 0 {
		t.Fatalf("add with password → password creds; got %+v", c)
	}
	// Add flow: a submitted key ⇒ key auth.
	if c := creds(s.resolveEditSecret(ssh, pem, "p", "", "", nil)); string(c.Key) != pem || c.Passphrase != "p" {
		t.Fatalf("add with key → key creds; got %+v", c)
	}

	// Non-SSH (mTLS): blank keeps the stored bundle; a new bundle replaces it.
	bundle := []byte("CA---CERT---KEY")
	mtls := &store.Node{Transport: dockercli.TransportMTLS, SecretEnc: SealNodeSecret(bundle, key)}
	if got := s.resolveEditSecret(dockercli.TransportMTLS, "", "", "", "", mtls); !bytes.Equal(got, bundle) {
		t.Fatalf("mTLS blank must keep stored bundle; got %q", got)
	}
	if got := s.resolveEditSecret(dockercli.TransportMTLS, "NEW", "", "", "", mtls); string(got) != "NEW" {
		t.Fatalf("mTLS new bundle must replace; got %q", got)
	}

	// --- Switching transport must NOT carry the old-transport credential over. ---
	// SSH key node → socket-proxy (tcp): no secret needed, and the old key is dropped.
	if got := s.resolveEditSecret(dockercli.TransportTCPProxy, "", "", "", "", keyNode); len(got) != 0 {
		t.Fatalf("ssh→tcp must clear the stored key, got %d bytes: %q", len(got), got)
	}
	// SSH key node → mTLS with a fresh bundle: uses the bundle, not the old SSH key.
	if got := s.resolveEditSecret(dockercli.TransportMTLS, "BUNDLE", "", "", "", keyNode); string(got) != "BUNDLE" {
		t.Fatalf("ssh→mtls must use the new bundle; got %q", got)
	}
	// tcp node → SSH with a fresh key: uses the key (no stored SSH creds to keep).
	tcpNode := &store.Node{Transport: dockercli.TransportTCPProxy}
	if c := creds(s.resolveEditSecret(ssh, pem, "", "", "", tcpNode)); string(c.Key) != pem {
		t.Fatalf("tcp→ssh must use the submitted key; got %+v", c)
	}
	// tcp node → SSH with a blank key is intentionally empty (user must supply creds),
	// never silently reused from another transport.
	if c := creds(s.resolveEditSecret(ssh, "", "", "", "", tcpNode)); len(c.Key) != 0 || c.Password != "" {
		t.Fatalf("tcp→ssh with blank creds must stay empty; got %+v", c)
	}
}

// TestSSHAuthMethodFor covers the pure method-selection: explicit request wins,
// else inferred from what was submitted, else from what is stored.
func TestSSHAuthMethodFor(t *testing.T) {
	keyStored := dockercli.SSHCreds{Key: []byte("k")}
	pwStored := dockercli.SSHCreds{Password: "p"}
	cases := []struct {
		req, secret, password string
		stored                dockercli.SSHCreds
		want                  string
	}{
		{"password", "", "", keyStored, "password"}, // explicit wins over stored key
		{"key", "", "", pwStored, "key"},            // explicit wins over stored password
		{"", "", "pw", keyStored, "password"},       // submitted password infers password
		{"", "newkey", "", pwStored, "key"},         // submitted key infers key
		{"", "", "", pwStored, "password"},          // blank edit → infer from stored password
		{"", "", "", keyStored, "key"},              // blank edit → infer from stored key
		{"", "", "", dockercli.SSHCreds{}, "key"},   // nothing → default key
	}
	for i, c := range cases {
		if got := sshAuthMethodFor(c.req, c.secret, c.password, c.stored); got != c.want {
			t.Errorf("case %d: sshAuthMethodFor(%q,%q,%q,…)=%q want %q", i, c.req, c.secret, c.password, got, c.want)
		}
	}
}

// purgeNode must page a large per-node catalog rather than materialising every
// row (with its JSON blobs) at once — a node with thousands of backups could
// otherwise OOM the daemon mid-purge (Step 12 / finding #11). With more rows
// than one batch, the loop must delete them ALL and terminate.
func TestPurgeNodePagesLargeCatalog(t *testing.T) {
	st := testStore(t)
	sb, err := storage.NewLocal(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store:    st,
		reg:      dockercli.NewRegistry(),
		engine:   &backup.Engine{Store: st, Storage: sb, Log: func(string, string, string) {}},
		machines: newMachineCache(),
	}
	if err := st.UpsertNode(&store.Node{ID: "n1", Name: "big", Transport: "ssh", Address: "x"}); err != nil {
		t.Fatal(err)
	}

	const n = 450 // > 2 * purgeBatch, so the loop makes several passes
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("b-%03d", i)
		if err := st.CreateBackup(&store.Backup{ID: id, NodeID: "n1", TargetName: "app", Status: "success", CreatedAt: int64(i)}); err != nil {
			t.Fatal(err)
		}
		// A unique, non-existent storage key: DeleteArtifacts then targets a file
		// that isn't there (a clean no-op) instead of the storage root.
		_ = st.SetBackupStorageKey(id, "k-"+id)
	}
	// Another node's backup must be left completely untouched.
	_ = st.CreateBackup(&store.Backup{ID: "keep", NodeID: "n2", TargetName: "app", Status: "success", CreatedAt: 1})

	s.purgeNode("n1") // must terminate (not hang) and remove everything for n1

	if left, _ := st.ListBackups("n1", 100000); len(left) != 0 {
		t.Fatalf("expected all n1 backups purged, %d remain", len(left))
	}
	if other, _ := st.ListBackups("n2", 10); len(other) != 1 {
		t.Fatalf("another node's backups must be untouched, got %d", len(other))
	}
	if _, err := st.GetNode("n1"); err == nil {
		t.Fatal("the node row itself should be deleted last")
	}
}
