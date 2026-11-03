#!/usr/bin/env sh
# govulncheck gate (PLAN §10.2). Fails on ANY reachable Go vulnerability EXCEPT a
# small, documented allowlist of advisories that currently have NO upstream fix.
# Everything fixable still blocks the build. Re-review the allowlist each release.
set -eu

# Accepted github.com/docker/docker advisories (no fixed release of that module;
# the fixes are in the daemon module, github.com/moby/moby/v2).
# Owner: the release maintainer. Last reviewed 2026-11-03; review again every release.
#
# All five are in DAEMON code, which the DockBack client never runs:
#   GO-2026-5746, 5668, 5617: the daemon's container-archive (`docker cp`)
#     handling. Not reported as reachable on 2026-11-03.
#   GO-2026-4887, 4883: AuthZ-plugin and plugin-privilege checks. Reported as
#     reachable only because the advisory names the whole module, not functions.
#
# The socket-proxy is not the reason. It passes allowed requests through, so it
# does not protect a vulnerable daemon on a node. Each node's Docker Engine must be
# patched: 29.5.1 fixes the archive ones, and 29.3.1 the AuthZ/plugin ones. Remove
# an entry the moment a fixed client module ships, then bump.
ALLOW="
GO-2026-5746
GO-2026-5668
GO-2026-5617
GO-2026-4887
GO-2026-4883
"

# Exit status: 0 the scan completed and nothing unaccepted is reachable; 1 it
# completed and something is; 2 it did not complete, which says nothing either way.
EXIT_UNACCEPTED=1
EXIT_INCOMPLETE=2
# govulncheck's own status for "completed, and found vulnerabilities".
SCANNER_FOUND=3

GOVULN="${GOVULN:-golang.org/x/vuln/cmd/govulncheck@v1.9.0}"

incomplete() {
	echo ""
	echo "✗ govulncheck did not complete: $1"
	exit "$EXIT_INCOMPLETE"
}

# Built and run as a binary: `go run` reports every failure as status 1, which
# would make "found vulnerabilities" and "crashed" the same answer.
tooldir=$(mktemp -d)
trap 'rm -rf "$tooldir"' EXIT
GOBIN="$tooldir" go install "$GOVULN" || incomplete "could not build $GOVULN"

status=0
out=$("$tooldir/govulncheck" ./... 2>&1) || status=$?
printf '%s\n' "$out"

ids=""
case "$status" in
0)
	printf '%s\n' "$out" | grep -q '^No vulnerabilities found\.$' \
		|| incomplete "it exited 0 without saying the scan was clean"
	;;
"$SCANNER_FOUND")
	# The summary comes last and states the count, so a report cut short shows here.
	reported=$(printf '%s\n' "$out" | sed -n 's/^Your code is affected by \([0-9][0-9]*\) vulnerabilit.*/\1/p')
	ids=$(printf '%s\n' "$out" | sed -n 's/^Vulnerability #[0-9][0-9]*: \(GO-[0-9]\{4\}-[0-9][0-9]*\).*/\1/p' | sort -u)
	listed=$(printf '%s\n' "$ids" | grep -c 'GO-' || true)
	[ -n "$reported" ] || incomplete "its report has no summary"
	[ "$reported" -gt 0 ] && [ "$reported" -eq "$listed" ] \
		|| incomplete "its summary says ${reported} but it listed ${listed}"
	;;
*)
	incomplete "it exited with status $status"
	;;
esac

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
	exit "$EXIT_UNACCEPTED"
fi

n=$(printf '%s\n' "$allow" | grep -c 'GO-' || true)
echo ""
echo "✓ govulncheck: the scan completed with no unaccepted vulnerabilities (${n} accepted/no-fix advisories ignored)"
