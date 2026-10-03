package config

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
)

func TestSelfSignedTLSConfig(t *testing.T) {
	tc, err := SelfSignedTLSConfig("10.168.1.50")
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.Certificates) != 1 {
		t.Fatalf("want 1 certificate, got %d", len(tc.Certificates))
	}
	if tc.MinVersion < tls.VersionTLS12 {
		t.Fatal("min TLS version should be >= 1.2")
	}
	leaf, err := x509.ParseCertificate(tc.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	wantIPs := []net.IP{net.IPv4(127, 0, 0, 1), net.ParseIP("10.168.1.50")}
	for _, want := range wantIPs {
		found := false
		for _, ip := range leaf.IPAddresses {
			if ip.Equal(want) {
				found = true
			}
		}
		if !found {
			t.Errorf("certificate missing SAN IP %v", want)
		}
	}
}

func TestTLSConfigPairing(t *testing.T) {
	t.Setenv("DOCKBACK_TLS_CERT", "/etc/ssl/cert.pem") // key intentionally unset
	if _, err := Load(); err == nil {
		t.Fatal("expected an error when TLS cert is set without a key")
	}

	t.Setenv("DOCKBACK_TLS_KEY", "/etc/ssl/key.pem")
	c, err := Load()
	if err != nil {
		t.Fatalf("cert+key together should load: %v", err)
	}
	if !c.TLSEnabled() {
		t.Fatal("TLSEnabled() should be true with cert+key")
	}
}
