package backup

import (
	"fmt"
	"strings"
)

// Requiring write-only encryption, per container (F163).
//
// Write-only mode is global: one offline public key, every new backup sealed to
// it. That is the right shape for the feature, and it leaves one gap.
//
// The gap is that it can be turned OFF. Legitimately, too — an operator disables
// it to run a restore drill, because an instance that cannot read its own
// backups cannot test them. Then the next scheduled run of every container
// writes an archive the running server can open. For most containers that is
// simply the default and nobody minds.
//
// For a store of credentials to OTHER systems it is different in kind. Termix is
// the sharpest case in the fleet: its database holds SSH credentials and private
// keys for a whole set of machines, and — because the application keeps the key
// that decrypts that database in the same directory as the database — a copy of
// its data directory IS those credentials. The backup's own encryption is the
// only barrier there is, and an archive sealed to the master key puts that
// barrier on the same server an attacker would already be standing on.
//
// So a container can be marked as one that must never be backed up any other
// way. When it is, a run that would produce a master-key-openable archive is
// REFUSED, before anything is captured.
//
// Three deliberate choices:
//
//   - It is OPT-IN, default off. Refusing by default would mean an operator who
//     has not yet set up an offline key gets no backup at all of the thing they
//     can least afford to lose — which is a worse outcome, not a safer one.
//   - It FAILS the run rather than downgrading it. The whole content of the
//     setting is "I would rather have no backup than a readable one"; quietly
//     producing the readable one would make it meaningless.
//   - It is checked BEFORE any capture, so a refusal costs nothing and touches
//     nothing.

// requireWriteOnlyKey is the per-container setting, following the same shape as
// every other per-container backup choice so scheduled and manual runs agree.
const requireWriteOnlyKey = "backup.require_write_only"

func requireWriteOnlyKeyFor(nodeID, name string) string {
	return requireWriteOnlyKey + "." + nodeID + "." + name
}

// RequireWriteOnly reports whether this container refuses to be backed up
// without write-only encryption (F163). Default off.
func (e *Engine) RequireWriteOnly(nodeID, name string) bool {
	v, _ := e.Store.GetSetting(requireWriteOnlyKeyFor(nodeID, name), "false")
	return v == "true"
}

// SetRequireWriteOnly stores the choice, applying to this container's next
// backup whether it is scheduled or manual.
func (e *Engine) SetRequireWriteOnly(nodeID, name string, on bool) error {
	if !on {
		return e.Store.SetSetting(requireWriteOnlyKeyFor(nodeID, name), "")
	}
	return e.Store.SetSetting(requireWriteOnlyKeyFor(nodeID, name), "true")
}

// writeOnlyRequirement returns the error that refuses a run, or nil.
//
// The message has to carry the whole decision, because it is read by somebody
// who did not necessarily set the flag and is now looking at a failed backup: it
// says what was refused, why this container in particular, and both ways out.
func (e *Engine) writeOnlyRequirement(nodeID, name, image string) error {
	if !e.RequireWriteOnly(nodeID, name) || e.WriteOnlyEnabled() {
		return nil
	}
	// The lead names why THIS container in one clause. The credential-store text
	// is a paragraph that ends in recommendations about write-only — splicing all
	// of it in here produces a wall of text that repeats what the rest of this
	// message is already saying, so only its first statement is used.
	why := "This container is marked as one that must only be backed up with write-only encryption"
	if p := ProfileFor(image); p != nil && p.CredentialStore != "" {
		why = firstStatement(p.CredentialStore) + ", and it is marked as a container that must only be backed up with write-only encryption"
	}
	return fmt.Errorf("%s — which is currently OFF. "+
		"An archive taken now could be opened by this server, and that is the protection the setting exists to keep. "+
		"Turn write-only back on in Settings, or clear the requirement on this container if a master-key-encrypted archive is acceptable. "+
		"Nothing was captured", why)
}

// WriteOnlyRequirementFor is the API-facing view: the reason this container's
// next backup would be refused, or "" when it would run. Lets the UI say so
// before the operator finds out from a failed scheduled run at three in the
// morning.
func (e *Engine) WriteOnlyRequirementFor(nodeID, name, image string) string {
	if err := e.writeOnlyRequirement(nodeID, name, image); err != nil {
		return err.Error()
	}
	return ""
}

// firstStatement takes the leading clause of a multi-sentence note — up to the
// first em-dash aside or full stop — so a paragraph written for a settings page
// can be reused as the opening of a one-line refusal without dragging its
// recommendations along.
func firstStatement(s string) string {
	if i := strings.Index(s, " — "); i > 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i]
	}
	return strings.TrimRight(strings.TrimSpace(s), ".")
}
