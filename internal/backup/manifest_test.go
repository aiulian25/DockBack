package backup

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestManifestContractFields locks in PLAN §4.12: the manifest is the full,
// versioned contract — image digest, volume list + checksum, DB engine/version,
// key fingerprint, size, timestamps, and the verification report all round-trip.
func TestManifestContractFields(t *testing.T) {
	m := Manifest{
		Version: ManifestVersion, BackupID: "b1", CreatedAt: nowRFC3339(),
		Stack: "karakeep", Service: "web", TargetName: "karakeep-web-1",
		Image: "ghcr.io/x/karakeep:0.16.0", ImageDigest: "ghcr.io/x/karakeep@sha256:abc",
		Volumes:       []VolumeRef{{Name: "data", Destination: "/data", Type: "volume"}},
		VolumesSHA256: "deadbeef",
		Databases:     []DBDump{{Service: "db", Engine: "postgres", Version: "pg_dumpall (PostgreSQL) 16.2", Path: "db/db.dump", Bytes: 1234}},
		CipherSHA256:  "cafef00d", CipherSize: 4096, KeyFingerprint: "617dee66cfed",
		Verification: &VerificationReport{OK: true, At: nowRFC3339(), Checks: []Check{{Name: "ciphertext-sha256", OK: true}}},
	}

	js, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"volumes_sha256":"deadbeef"`, `"version":"pg_dumpall`, `"image_digest"`, `"key_fingerprint"`, `"verification"`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("manifest JSON missing %s:\n%s", want, js)
		}
	}

	var back Manifest
	if err := json.Unmarshal(js, &back); err != nil {
		t.Fatal(err)
	}
	if back.VolumesSHA256 != "deadbeef" {
		t.Errorf("volumes checksum lost: %q", back.VolumesSHA256)
	}
	if len(back.Databases) != 1 || back.Databases[0].Version == "" {
		t.Errorf("DB version lost: %+v", back.Databases)
	}
	if back.Verification == nil || !back.Verification.OK {
		t.Errorf("verification report lost: %+v", back.Verification)
	}
}

// TestManifestOriginalComposeFlag (F57) locks the has_original_compose flag: it
// round-trips when set, and is OMITTED (omitempty) when false so non-SSH backups'
// manifests are byte-identical to before.
func TestManifestOriginalComposeFlag(t *testing.T) {
	on, _ := json.Marshal(Manifest{HasOriginalCompose: true})
	if !strings.Contains(string(on), `"has_original_compose":true`) {
		t.Errorf("flag not serialized when set:\n%s", on)
	}
	var back Manifest
	_ = json.Unmarshal(on, &back)
	if !back.HasOriginalCompose {
		t.Error("flag lost on round-trip")
	}
	off, _ := json.Marshal(Manifest{HasOriginalCompose: false})
	if strings.Contains(string(off), "has_original_compose") {
		t.Errorf("flag must be omitted when false (byte-identical to old backups):\n%s", off)
	}
}
