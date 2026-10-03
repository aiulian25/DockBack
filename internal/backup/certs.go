package backup

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Certificate custody and verification (F143).
//
// For most applications a TLS certificate is somebody else's problem: a reverse
// proxy terminates HTTPS and the app never sees one. For the proxy itself the
// certificates ARE part of the state, and they fail in a way no other file does
// — they expire. A restore can be byte-perfect and still put an unusable
// deployment back, because the certificate it faithfully restored stopped being
// valid two months ago.
//
// Three things follow, and DockBack does each of them:
//
//  1. Record what the archive holds, so an operator can see at a glance whether
//     the copy they are about to restore contains a certificate that is still
//     good.
//  2. After a restore, prove the files actually landed. A missing certificate is
//     the difference between a proxy that serves and one that will not start,
//     and it is worth failing over rather than discovering from a health gate.
//  3. Say when a restored certificate has expired — and do NOT fail over that.
//     The restore did its job; the certificate aged. Refusing would block the
//     disaster-recovery case this whole product exists for, and the fix (renew)
//     needs the deployment running.
//
// The private key is never read. Its presence beside the certificate is
// established by testing for the file, so the archive can state that the pair
// travelled together without the key's bytes passing through anything.

// certExpiryWarnDays is how far ahead a restored certificate's expiry is worth
// mentioning. Let's Encrypt certificates are 90-day and renew at 30 days left,
// so a restored copy inside that window is normal — but on a machine that has
// just been rebuilt it is also the window in which nobody has yet checked that
// renewals work there.
const certExpiryWarnDays = 30

// inventoryCertificates records the certificates inside the captured data
// (F143). Entirely best-effort: an application with no declared certificate
// roots does no work at all, and a scan that cannot run records nothing rather
// than guessing.
func (e *Engine) inventoryCertificates(ctx context.Context, cli *client.Client, containerID, image string, man *Manifest, logID string) {
	roots := CertificateRootsFor(image)
	if len(roots) == 0 {
		return
	}
	files, err := dockercli.ReadCertificatesBounded(ctx, cli, containerID, roots)
	if err != nil {
		e.logf(logID, "INFO", "Certificate scan skipped (%v) — the certificate files are still captured with the rest of the data", err)
		return
	}
	if len(files) == 0 {
		return
	}
	refs := certRefsOf(files)
	if len(refs) == 0 {
		return
	}
	man.Certificates = refs

	now := time.Now()
	var expired, soon, keyless int
	for _, c := range refs {
		switch {
		case certExpired(c, now):
			expired++
		case certExpiringWithin(c, now, certExpiryWarnDays):
			soon++
		}
		if !c.HasKey {
			keyless++
		}
	}
	e.logf(logID, "INFO", "Captured %d TLS certificate(s) with the data%s", len(refs), certSummary(expired, soon, keyless))
	if expired > 0 {
		e.logf(logID, "WARN", "%d captured certificate(s) have already expired — this backup is still correct, but restoring it will restore an expired certificate. Renew before relying on it for recovery.", expired)
	}
}

// certSummary renders the counts as a short clause, or nothing when there is
// nothing to report — a backup log should not announce that everything is fine
// in three different ways.
func certSummary(expired, soon, keyless int) string {
	var parts []string
	if expired > 0 {
		parts = append(parts, fmt.Sprintf("%d already expired", expired))
	}
	if soon > 0 {
		parts = append(parts, fmt.Sprintf("%d expiring within %d days", soon, certExpiryWarnDays))
	}
	if keyless > 0 {
		parts = append(parts, fmt.Sprintf("%d with no private key beside them", keyless))
	}
	if len(parts) == 0 {
		return ""
	}
	return " — " + strings.Join(parts, ", ")
}

// certRefsOf parses each captured certificate into the manifest record.
//
// A file that will not parse is dropped rather than recorded as unknown: the
// scan matches by NAME, so it can legitimately pick up something that is not a
// certificate at all, and recording that as a certificate DockBack could not
// read would invent a problem.
//
// Certbot's layout stores the SAME certificate twice in one directory —
// cert.pem is the leaf, fullchain.pem is the leaf plus its intermediates — so a
// scan by filename finds one certificate and reports two. Counting them twice
// would make "2 certificates expiring" out of one, which is worse than useless.
// fullchain.pem wins because it is the file a server is actually configured to
// load.
func certRefsOf(files []dockercli.CertFile) []CertRef {
	best := map[string]CertRef{}
	for _, f := range files {
		ref, ok := certRefOf(f)
		if !ok {
			continue
		}
		// Same directory, same validity, same issuer, same name count: the same
		// certificate under two names.
		key := strings.Join([]string{filepath.Dir(ref.Path), ref.NotBefore, ref.NotAfter, ref.Issuer, strconv.Itoa(ref.Names)}, "\x00")
		if prev, dup := best[key]; dup && !preferredCertFile(ref.Path, prev.Path) {
			continue
		}
		best[key] = ref
	}
	out := make([]CertRef, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// preferredCertFile reports whether a should replace b as the record for one
// certificate: fullchain first, then the shorter path, then alphabetical — the
// last two only so the choice is deterministic rather than dependent on the
// order `find` happened to walk the directory.
func preferredCertFile(a, b string) bool {
	af, bf := filepath.Base(a) == "fullchain.pem", filepath.Base(b) == "fullchain.pem"
	if af != bf {
		return af
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// certRefOf parses the LEAF certificate of a PEM file. A fullchain.pem holds the
// leaf followed by its intermediates; the leaf is the one whose expiry decides
// whether the deployment works.
func certRefOf(f dockercli.CertFile) (CertRef, bool) {
	block, _ := pem.Decode(f.PEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return CertRef{}, false
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CertRef{}, false
	}
	issuer := strings.TrimSpace(c.Issuer.CommonName)
	if issuer == "" {
		issuer = strings.Join(c.Issuer.Organization, " ")
	}
	// Self-signed is worth distinguishing: it never renews on its own, so a
	// restore onto a new machine is exactly when it gets forgotten.
	if c.Issuer.String() == c.Subject.String() {
		issuer = "self-signed"
	}
	return CertRef{
		Path:      f.Path,
		Issuer:    strings.TrimSpace(issuer),
		NotBefore: c.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:  c.NotAfter.UTC().Format(time.RFC3339),
		Names:     len(c.DNSNames),
		HasKey:    f.HasKey,
	}, true
}

// certExpired reports whether a recorded certificate's validity has passed. An
// unparseable or absent date yields false — an unreadable date is not evidence
// of anything, and treating it as expiry would invent failures.
func certExpired(c CertRef, now time.Time) bool {
	t, ok := certNotAfter(c)
	return ok && now.After(t)
}

// certExpiringWithin reports whether a certificate expires inside the window.
func certExpiringWithin(c CertRef, now time.Time, days int) bool {
	t, ok := certNotAfter(c)
	return ok && !now.After(t) && t.Sub(now) < time.Duration(days)*24*time.Hour
}

func certNotAfter(c CertRef) (time.Time, bool) {
	if c.NotAfter == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, c.NotAfter)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// assertCertificatesRestored proves the certificates this backup recorded are
// back on disk, BEFORE the container is started (F143).
//
// Deliberately asymmetric about what it will fail on, following the same rule as
// the SQLite contract:
//
//   - a recorded certificate that is NOT on disk, or no longer parses — a
//     measured fact, and the exact condition behind nginx's "cannot load
//     certificate". Fatal.
//   - a recorded certificate whose private key is gone — equally fatal: a
//     certificate without its key cannot serve anything.
//   - an EXPIRED certificate — reported loudly, never fatal. The restore is
//     correct; the certificate aged. Failing here would block the recovery of an
//     old backup, which is when this product matters most, and the fix needs the
//     deployment running.
//   - a scan that could not run, or a backup that recorded nothing — logged,
//     never failed. A check that could not run must not masquerade as one that
//     passed, in either direction.
func (e *Engine) assertCertificatesRestored(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) error {
	if man == nil || len(man.Certificates) == 0 {
		return nil
	}
	roots := CertificateRootsFor(manifestImage(man, b))
	if len(roots) == 0 {
		return nil
	}
	files, err := dockercli.ReadCertificatesBounded(ctx, cli, opts.TargetID, roots)
	if err != nil {
		e.logf(b.ID, "WARN", "Restored %d certificate(s) but could not read them back to verify (%v) — the files were restored, but their presence on the target was not confirmed", len(man.Certificates), err)
		return nil
	}
	found := map[string]dockercli.CertFile{}
	for _, f := range files {
		found[f.Path] = f
	}

	now := time.Now()
	var failures, expiredPaths []string
	ok := 0
	for _, want := range man.Certificates {
		f, present := found[want.Path]
		if !present {
			failures = append(failures, fmt.Sprintf("%s is not on the target after the restore", want.Path))
			continue
		}
		got, parsed := certRefOf(f)
		if !parsed {
			failures = append(failures, fmt.Sprintf("%s came back but is not a readable certificate", want.Path))
			continue
		}
		if want.HasKey && !f.HasKey {
			failures = append(failures, fmt.Sprintf("%s came back without its private key, which was captured with it — a certificate cannot serve anything without its key", want.Path))
			continue
		}
		if certExpired(got, now) {
			expiredPaths = append(expiredPaths, want.Path+" (expired "+got.NotAfter+")")
			continue
		}
		ok++
	}

	for _, p := range expiredPaths {
		e.logf(b.ID, "WARN", "Restored certificate %s has expired. The restore is correct — the certificate aged — but clients will refuse it until it is renewed. Renewal runs from the restored configuration; confirm this machine can reach the certificate authority (and your DNS provider's API, if you use DNS-01 validation).", p)
	}
	if len(failures) > 0 {
		for _, f := range failures {
			e.logf(b.ID, "ERROR", "Certificate restore verification FAILED: %s", f)
		}
		return fmt.Errorf("restored certificates did not come back intact: %s", strings.Join(failures, "; "))
	}
	if ok > 0 {
		e.logf(b.ID, "INFO", "Verified %d certificate(s) on the target: present, readable and unexpired", ok)
	}
	return nil
}
