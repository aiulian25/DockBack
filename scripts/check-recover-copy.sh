#!/usr/bin/env sh
# Fails if a second copy of the offline recovery tool exists.
#
# Only scripts/dockback-recover.py is embedded (embed_recover.go) and only its
# SHA-256 is served as the recovery-kit fingerprint (internal/api/recovery.go).
# A second copy is a file someone can edit believing it ships — and this is the
# one file that has to work when nothing else does.
set -eu
extra=$(find . -name 'dockback-recover.py' -not -path './scripts/dockback-recover.py' -not -path './.git/*')
if [ -n "$extra" ]; then
  echo "error: duplicate copy of the offline recovery tool:" >&2
  echo "$extra" >&2
  echo "only scripts/dockback-recover.py is embedded and fingerprinted." >&2
  exit 1
fi
echo "ok: exactly one dockback-recover.py"
