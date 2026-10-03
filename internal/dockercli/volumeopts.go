package dockercli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Named-volume driver options (F90).
//
// A volume's DRIVER was recorded but the options that make the driver mean
// anything were not. A named volume created with the local driver and NFS
// options (`type=nfs,o=addr=…,device=:/export`) was therefore recreated on
// restore as a plain empty local volume — and the restored data then went to
// local disk instead of the NAS, silently.
//
// CREDENTIALS ARE NOT RECORDED. CIFS/SMB volumes routinely carry a password in
// their options (`o=username=backup,password=…`), and the manifest is not a safe
// place for one: the sidecar written beside every archive is READABLE by default
// (manifest.encrypt is off unless turned on), so it travels in the clear to S3,
// SMB, WebDAV and SFTP destinations, and it also sits in plaintext in the local
// catalog. Recording a password there would turn a convenience feature into a
// credential leak on every destination the backup reaches.
//
// So secret-looking values are replaced with a marker and their keys listed, and
// the restore refuses to half-create such a volume — it says exactly which
// options are missing and leaves the operator to create it, rather than silently
// producing a volume that mounts the wrong thing or nothing at all.

// redactedMarker replaces a value that was deliberately not recorded. Chosen to
// be obviously not-a-password if it ever reaches a config by mistake.
const redactedMarker = "<redacted-by-dockback>"

// secretOptionKeys are option keys whose VALUE is a credential.
var secretOptionKeys = []string{"password", "passwd", "secret", "token", "key", "credential", "credentials"}

// isSecretKey reports whether an option key names a credential. Substring
// matching on a lowercased key, so `o.password`, `PASSWORD` and
// `driver-password` are all caught — over-redacting a harmless field costs a
// warning; under-redacting costs a leaked password.
func isSecretKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range secretOptionKeys {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
}

// RedactVolumeOptions returns options safe to record, plus the keys whose values
// were withheld.
//
// The local driver packs its real mount options into a single comma-separated
// `o` value (`addr=10.0.0.5,username=u,password=p`), so that string is walked
// item by item rather than treated as one opaque value — otherwise a single
// password would force the whole mount spec to be dropped.
func RedactVolumeOptions(opts map[string]string) (safe map[string]string, redacted []string) {
	if len(opts) == 0 {
		return nil, nil
	}
	safe = make(map[string]string, len(opts))
	for k, v := range opts {
		if isSecretKey(k) {
			safe[k] = redactedMarker
			redacted = append(redacted, k)
			continue
		}
		if k == "o" || k == "opts" {
			cleaned, hits := redactMountOptionList(v)
			safe[k] = cleaned
			for _, h := range hits {
				redacted = append(redacted, k+"."+h)
			}
			continue
		}
		safe[k] = v
	}
	sort.Strings(redacted)
	return safe, redacted
}

// redactMountOptionList walks a comma-separated `key=value,flag,…` mount option
// string, replacing credential values and preserving everything else.
func redactMountOptionList(v string) (string, []string) {
	parts := strings.Split(v, ",")
	var hits []string
	for i, p := range parts {
		k, _, ok := strings.Cut(p, "=")
		if !ok || !isSecretKey(k) {
			continue
		}
		parts[i] = k + "=" + redactedMarker
		hits = append(hits, strings.TrimSpace(k))
	}
	return strings.Join(parts, ","), hits
}

// HasRedactedOptions reports whether any recorded option value was withheld, so
// a restore knows it cannot faithfully recreate this volume.
func HasRedactedOptions(opts map[string]string) bool {
	for _, v := range opts {
		if strings.Contains(v, redactedMarker) {
			return true
		}
	}
	return false
}

// InspectVolume returns a named volume's driver, options and labels.
func InspectVolume(ctx context.Context, c *client.Client, name string) (driver string, opts, labels map[string]string, err error) {
	v, err := c.VolumeInspect(ctx, name)
	if err != nil {
		return "", nil, nil, err
	}
	return v.Driver, copyMap(v.Options), copyMap(v.Labels), nil
}

// ErrVolumeOptionsMismatch is the sentinel for a volume that already exists with
// different driver options — restoring into it would write to the wrong backing
// store, which is precisely the failure this feature exists to prevent.
var ErrVolumeOptionsMismatch = errors.New("volume exists with different driver options")

// VolumeOptionsMismatchError names the differing options so the operator can act.
type VolumeOptionsMismatchError struct {
	Name string
	Diff []string
}

func (e *VolumeOptionsMismatchError) Error() string {
	return fmt.Sprintf(
		"volume %s exists with different driver options (%s) — restoring into it would write to the wrong backing store",
		e.Name, strings.Join(e.Diff, "; "))
}

func (e *VolumeOptionsMismatchError) Unwrap() error { return ErrVolumeOptionsMismatch }

// EnsureVolume creates a named volume with its recorded driver, options and
// labels, or verifies an existing one matches.
//
// It never MUTATES an existing volume — Docker offers no way to, and silently
// reusing one backed by different storage is the exact silent-wrong-place
// failure being fixed. A mismatch is refused and named.
func EnsureVolume(ctx context.Context, c *client.Client, name, driver string, opts, labels map[string]string) error {
	if name == "" {
		return nil
	}
	if existing, err := c.VolumeInspect(ctx, name); err == nil {
		if diff := diffVolumeOptions(opts, existing.Options); len(diff) > 0 {
			return &VolumeOptionsMismatchError{Name: name, Diff: diff}
		}
		return nil
	}
	// A volume whose options were partially withheld cannot be recreated
	// faithfully. Creating it with the credential missing would produce a volume
	// that fails to mount — or worse, one that quietly falls back to local disk.
	// Refuse, and say what is needed.
	if HasRedactedOptions(opts) {
		return &VolumeOptionsMismatchError{
			Name: name,
			Diff: []string{"its options include a credential that DockBack deliberately does not store; create this volume by hand with its driver options before restoring"},
		}
	}
	_, err := c.VolumeCreate(ctx, volume.CreateOptions{
		Name: name, Driver: driver, DriverOpts: opts, Labels: labels,
	})
	return err
}

// diffVolumeOptions lists the recorded options an existing volume does not
// match. A recorded option that was REDACTED is skipped — we withheld it, so we
// are in no position to complain that it differs.
func diffVolumeOptions(want, got map[string]string) []string {
	var diff []string
	for k, wv := range want {
		if strings.Contains(wv, redactedMarker) {
			continue
		}
		gv, ok := got[k]
		if !ok {
			diff = append(diff, fmt.Sprintf("%s recorded as %q, absent on this host", k, wv))
			continue
		}
		if gv != wv {
			diff = append(diff, fmt.Sprintf("%s recorded as %q, host has %q", k, wv, gv))
		}
	}
	sort.Strings(diff)
	return diff
}
