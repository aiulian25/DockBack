package backup

import (
	"encoding/hex"
	"strings"

	"dockback/internal/crypto"
)

// Operator-recorded Redis password, per container (F205).
//
// THE LOOP THIS CLOSES
//
// The Redis dump searches everywhere a password can normally be found (F180):
// the container's environment, the _FILE variants, REDIS_ARGS, a redis.conf, and
// the server's own command line. When none of them has one, the backup falls
// back to copying /data as files (F185/F191) — a correct fallback, since the RDB
// in there is a complete artifact at Redis's own chosen moment.
//
// But one arrangement defeats every one of those searches: a password set at
// RUNTIME with `CONFIG SET requirepass`. It is not in the environment, not in a
// file, not on the command line — it exists only inside the running server. So
// the fallback is not an occasional degradation for that broker, it is EVERY
// RUN, forever: each backup silently loses point-in-time consistency and grades
// lower, and the only notice is a log line telling the operator to set
// REDIS_PASSWORD — advice that does not apply to how they configured it.
//
// There was no place to record the answer once. Now there is.
//
// WHY IT IS SEALED, AND WHY IT IS WRITE-ONLY THROUGH THE API
//
// This is a live credential to a running service, so it is sealed under the
// master key exactly like a node's connection secret or the 2FA secret — never
// stored in plaintext, and never returned by the API. The container detail
// endpoint reports only WHETHER one is set. An operator who cannot remember the
// value re-enters it; an attacker who reaches the API reads nothing.
//
// It reaches redis-cli through REDISCLI_AUTH in the exec's environment — the
// same channel F180 already established, and specifically NOT the argv, so it
// never appears in the process list inside the container.

// ContainerRef identifies WHICH container a per-container setting belongs to.
//
// It exists because the dump path already carries two names that are easy to
// confuse — the compose SERVICE label and the container NAME — and every
// per-container setting in this app is keyed on the container name. Passing the
// pair as one named value rather than two loose strings makes it impossible to
// hand the dump path a service label and silently miss every recorded setting.
type ContainerRef struct {
	NodeID string
	Name   string // container name, as every per-container setting is keyed
}

// redisAuthKey is the per-container setting, following the same shape as every
// other per-container backup choice so scheduled and manual runs agree.
const redisAuthKey = "redis.auth"

// RedisAuthKeyPrefix is the settings prefix these live under, so key rotation
// can find every one of them without knowing which containers exist.
const RedisAuthKeyPrefix = redisAuthKey + "."

func redisAuthKeyFor(nodeID, name string) string {
	return RedisAuthKeyPrefix + nodeID + "." + name
}

// SetRedisAuth records (or clears) the password for one container's Redis.
//
// An empty value CLEARS the setting rather than sealing an empty string, so
// "remove this" and "set it to nothing" cannot diverge — a stored empty seal
// would look set to HasRedisAuth and contribute nothing to the dump.
func (e *Engine) SetRedisAuth(nodeID, name, password string) error {
	key := redisAuthKeyFor(nodeID, name)
	if strings.TrimSpace(password) == "" {
		return e.Store.SetSetting(key, "")
	}
	sealed, err := crypto.SealString(password, e.MasterKey())
	if err != nil {
		return err
	}
	return e.Store.SetSetting(key, hex.EncodeToString(sealed))
}

// HasRedisAuth reports whether a password is recorded, without opening it. This
// is what the API surfaces — the value itself never leaves the server.
func (e *Engine) HasRedisAuth(nodeID, name string) bool {
	v, _ := e.Store.GetSetting(redisAuthKeyFor(nodeID, name), "")
	return strings.TrimSpace(v) != ""
}

// redisAuth opens the recorded password for use in a dump. Returns "" when none
// is recorded OR when it cannot be opened — a password sealed under a master key
// this instance no longer has is, for every practical purpose, not recorded, and
// the dump's own search plus the file fallback still apply. It is deliberately
// silent about the failure at this level; the caller logs the outcome without
// ever naming the value.
func (e *Engine) redisAuth(nodeID, name string) string {
	v, _ := e.Store.GetSetting(redisAuthKeyFor(nodeID, name), "")
	if strings.TrimSpace(v) == "" {
		return ""
	}
	blob, err := hex.DecodeString(v)
	if err != nil {
		return ""
	}
	pw, err := crypto.OpenString(blob, e.MasterKey())
	if err != nil {
		return ""
	}
	return pw
}

// redisAuthEnv is the exec environment carrying an operator-recorded password,
// or nil when there is nothing recorded (the common case).
//
// The in-container search in redisDumpCommand skips itself entirely when
// REDISCLI_AUTH is already set, so recording a password short-circuits every
// other lookup rather than racing with it.
func (e *Engine) redisAuthEnv(nodeID, name string) []string {
	if pw := e.redisAuth(nodeID, name); pw != "" {
		return []string{"REDISCLI_AUTH=" + pw}
	}
	return nil
}

// withoutEnvKey drops every KEY=... entry for one variable from an exec
// environment, so a value that must win is not merely listed after a competing
// one. Two entries for the same name is an ambiguity, not an override.
func withoutEnvKey(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
