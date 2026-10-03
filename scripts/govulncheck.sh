#!/usr/bin/env sh
# govulncheck gate (PLAN §10.2). Fails on ANY reachable Go vulnerability EXCEPT a
# small, documented allowlist of advisories that currently have NO upstream fix.
# Everything fixable still blocks the build. Re-review the allowlist each release.
set -eu

# Accepted: github.com/docker/docker SDK advisories with "Fixed in: N/A" (no
# patched release). The Docker SDK is required to control Docker, and DockBack
# only ever reaches it through the hardened, allow-listed socket-proxy. Remove an
# entry the moment a fixed version ships, then bump the dependency.
ALLOW="
GO-2026-5746
GO-2026-5668
GO-2026-5617
GO-2026-4887
GO-2026-4883
"

out=$(GOFLAGS=-mod=mod go run golang.org/x/vuln/cmd/govulncheck@latest ./... 2>&1) || true
printf '%s\n' "$out"

ids=$(printf '%s\n' "$out" | grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u || true)
allow=$(printf '%s\n' "$ALLOW" | grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u || true)

fail=0
for id in $ids; do
	if ! printf '%s\n' "$allow" | grep -qx "$id"; then
		[ "$fail" -eq 0 ] && echo "" && echo "✗ govulncheck: unaccepted vulnerabilities:"
		echo "  - $id"
		fail=1
	fi
done

if [ "$fail" -ne 0 ]; then
	echo "Fix them, or (only if no upstream fix exists) add to the allowlist in scripts/govulncheck.sh with justification."
	exit 1
fi

n=$(printf '%s\n' "$allow" | grep -c 'GO-' || true)
echo ""
echo "✓ govulncheck: no unaccepted vulnerabilities (${n} accepted/no-fix advisories ignored)"
