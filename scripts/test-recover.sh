#!/usr/bin/env bash
# Proves the "restore without DockBack" promise (F35) end to end: it pulls a real
# .dback archive + manifest out of a running DockBack instance's backups volume,
# then recovers it on the HOST with nothing but python3 + scripts/dockback-recover.py
# — no DockBack process, no Docker daemon involvement in the decrypt — and asserts
# the plaintext tar comes back intact.
#
# Designed to be called from scripts/e2e.sh once a backup has verified (it reuses
# that drill's compose project + DOCKBACK_ENCRYPTION_KEY), but it also runs stand-
# alone against any compose-managed instance:
#
#   DOCKBACK_ENCRYPTION_KEY=<64hex> ./scripts/test-recover.sh [target-name-filter]
set -euo pipefail

cd "$(dirname "$0")/.."

PROJECT="${E2E_PROJECT:-dockback-e2e}"
COMPOSE_FILE="${E2E_COMPOSE_FILE:-scripts/e2e.compose.yml}"
SERVICE="${E2E_APP_SERVICE:-backup-app}"
FILTER="${1:-}"
: "${DOCKBACK_ENCRYPTION_KEY:?set DOCKBACK_ENCRYPTION_KEY to the 64-hex master key}"

rstep() { printf '\n\033[1;35m-- recover: %s --\033[0m\n' "$*"; }
rfail() { printf '\n\033[1;31mRECOVER TEST FAILED: %s\033[0m\n' "$*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

rstep "Locate the DockBack app container"
CID="$(docker compose -p "$PROJECT" -f "$COMPOSE_FILE" ps -q "$SERVICE" 2>/dev/null || true)"
[ -n "$CID" ] || rfail "could not find the '$SERVICE' container in project '$PROJECT'"

rstep "Copy the backups out of the instance (docker cp — no shell needed in the image)"
docker cp "$CID:/app/backups" "$WORK/backups" >/dev/null 2>&1 || rfail "docker cp of /app/backups failed"

# Pick the newest .dback (optionally filtered by the target name in its path).
mapfile -t ARCHIVES < <(find "$WORK/backups" -name '*.dback' -type f | { [ -n "$FILTER" ] && grep "$FILTER" || cat; } | sort)
[ "${#ARCHIVES[@]}" -gt 0 ] || rfail "no .dback archive found in the backups volume"
ARCHIVE="${ARCHIVES[-1]}"
echo "  archive: ${ARCHIVE#$WORK/}"

# Find its manifest sidecar (plain or sealed).
MANIFEST=""
for suf in .manifest.json .manifest.json.enc; do
  [ -f "${ARCHIVE}${suf}" ] && MANIFEST="${ARCHIVE}${suf}" && break
done
[ -n "$MANIFEST" ] || rfail "no manifest sidecar next to the archive"
echo "  manifest: ${MANIFEST#$WORK/}"

rstep "Recover on the host with python3 + dockback-recover.py (no DockBack, no Docker)"
OUT="$WORK/recovered.tar"
python3 scripts/dockback-recover.py \
  --key "$DOCKBACK_ENCRYPTION_KEY" \
  --in "$ARCHIVE" \
  --manifest "$MANIFEST" \
  --verify \
  --out "$OUT" || rfail "recovery script exited non-zero"

rstep "Assert the plaintext archive is intact"
[ -f "$OUT" ] || rfail "no output tar was produced"
LISTING="$(tar -tf "$OUT")"
echo "$LISTING" | sed 's/^/    /'
echo "$LISTING" | grep -q '^manifest.json$'  || rfail "recovered tar is missing manifest.json"
# A full backup carries volumes.tar; an incremental delta carries volumes-delta.tar (F61).
echo "$LISTING" | grep -Eq '^volumes(-delta)?\.tar$' || rfail "recovered tar is missing the volume payload"

# Incremental chain (F61): if any pulled backup is a delta, reconstruct its whole
# chain with --apply-chain and assert ordered tars + APPLY_ORDER.txt come out. This
# runs opportunistically — when the E2E produced an incremental delta — and is
# skipped (never failed) when only full backups are present, with a clear note.
rstep "Incremental chain: reconstruct full -> delta with --apply-chain (if a delta exists)"
DELTA=""
while IFS= read -r m; do
  if grep -q '"incremental":[[:space:]]*true' "$m" 2>/dev/null; then
    DELTA="${m%.manifest.json}"; DELTA="${DELTA%.manifest.json.enc}"; break
  fi
done < <(find "$WORK/backups" -name '*.manifest.json*' -type f | sort)
if [ -z "$DELTA" ]; then
  echo "  (no incremental delta in this backup set — chain case skipped)"
else
  echo "  delta archive: ${DELTA#$WORK/}"
  DMAN=""
  for suf in .manifest.json .manifest.json.enc; do [ -f "${DELTA}${suf}" ] && DMAN="${DELTA}${suf}" && break; done
  CHAINOUT="$WORK/chain"
  python3 scripts/dockback-recover.py \
    --key "$DOCKBACK_ENCRYPTION_KEY" \
    --in "$DELTA" --manifest "$DMAN" \
    --apply-chain "$WORK/backups" --out "$CHAINOUT" || rfail "--apply-chain exited non-zero"
  [ -f "$CHAINOUT/APPLY_ORDER.txt" ] || rfail "--apply-chain produced no APPLY_ORDER.txt"
  ls "$CHAINOUT"/00_*.tar >/dev/null 2>&1 || rfail "--apply-chain produced no full-baseline tar (00_*.tar)"
  NTARS="$(find "$CHAINOUT" -maxdepth 1 -name '*.tar' -type f | wc -l)"
  [ "$NTARS" -ge 2 ] || rfail "expected a chain of >=2 tars, got $NTARS"
  echo "  reconstructed $NTARS generation(s); APPLY_ORDER.txt written"
fi

# A wrong key must be rejected cleanly (no partial/garbage output).
rstep "Negative check: a wrong key must fail with no output"
WRONGKEY="$(printf '0%.0s' {1..64})"
if python3 scripts/dockback-recover.py --key "$WRONGKEY" --in "$ARCHIVE" --out "$WORK/wrong.tar" >/dev/null 2>&1; then
  rfail "a wrong key was accepted (must never happen)"
fi
[ -f "$WORK/wrong.tar" ] && rfail "a wrong key produced output (must never happen)"

printf '\n\033[1;32mRECOVER TEST PASSED: a .dback was decrypted + unpacked on the host without DockBack.\033[0m\n'
