package dockercli

import (
	"archive/tar"
	"bytes"
	"testing"
)

// The sidecar's output shape is a contract between a shell script and this
// parser, so it is worth pinning: an index line per file, a numbered .pem
// beside it, and nothing else trusted.
func certTar(t *testing.T, index string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if index != "" {
		write("./index.txt", index)
	}
	for n, b := range files {
		write("./"+n, b)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseCertTar(t *testing.T) {
	raw := certTar(t,
		"1\t/etc/letsencrypt/live/npm-4/fullchain.pem\t1\n2\t/data/custom_ssl/cert.pem\t0\n",
		map[string]string{"1.pem": "PEM-ONE", "2.pem": "PEM-TWO"})

	got, err := parseCertTar(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected two certificates, got %d", len(got))
	}
	byPath := map[string]CertFile{}
	for _, f := range got {
		byPath[f.Path] = f
	}
	le := byPath["/etc/letsencrypt/live/npm-4/fullchain.pem"]
	if string(le.PEM) != "PEM-ONE" || !le.HasKey {
		t.Errorf("certbot entry wrong: %+v", le)
	}
	if byPath["/data/custom_ssl/cert.pem"].HasKey {
		t.Error("a certificate with no key beside it must not claim one")
	}
}

func TestParseCertTarIgnoresUnexpectedMembers(t *testing.T) {
	// An index entry with no payload, a payload with no index entry, and a member
	// whose name is not the shape the sidecar produces. None may become a
	// certificate record — a scan that invents entries is worse than one that
	// finds nothing.
	raw := certTar(t, "1\t/a/fullchain.pem\t1\n3\t/c/fullchain.pem\t0\n",
		map[string]string{"1.pem": "ONE", "2.pem": "ORPHAN", "privkey.pem": "SECRET", "../escape.pem": "NOPE"})

	got, err := parseCertTar(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/a/fullchain.pem" {
		t.Fatalf("only the indexed, present certificate may be returned, got %+v", got)
	}
}

func TestParseCertTarEmpty(t *testing.T) {
	// The roots did not exist, or held nothing. "Found nothing" is a legitimate
	// answer and must not read as an error.
	got, err := parseCertTar(certTar(t, "", nil))
	if err != nil || len(got) != 0 {
		t.Fatalf("an empty scan must be empty and silent, got %v / %v", got, err)
	}
}
