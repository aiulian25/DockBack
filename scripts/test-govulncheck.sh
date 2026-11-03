#!/usr/bin/env sh
# Contract test for scripts/govulncheck.sh, using a fake toolchain, so it needs no
# network and no real scanner. The failure that matters is the wrapper passing when
# the scanner did not finish. The report text follows what govulncheck v1.9.0 prints.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
PASS=0
FAIL_UNACCEPTED=1
FAIL_INCOMPLETE=2
SCANNER_FOUND=3
ACCEPTED="GO-2026-4887"
UNACCEPTED="GO-2026-9999"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"

# The fake `go` builds a fake scanner that prints $FAKE_OUTPUT and exits $FAKE_STATUS.
cat > "$work/bin/go" <<'FAKE'
#!/usr/bin/env sh
[ -z "${FAKE_BUILD_FAILS:-}" ] || { echo "go: build failed" >&2; exit 1; }
cat > "$GOBIN/govulncheck" <<'SCANNER'
#!/usr/bin/env sh
printf '%s\n' "$FAKE_OUTPUT"
exit "$FAKE_STATUS"
SCANNER
chmod +x "$GOBIN/govulncheck"
FAKE
chmod +x "$work/bin/go"

report() {
	count=$1
	shift
	echo "=== Symbol Results ==="
	number=0
	for id in "$@"; do
		number=$((number + 1))
		printf '\nVulnerability #%s: %s\n    A description.\n  More info: https://pkg.go.dev/vuln/%s\n' "$number" "$id" "$id"
	done
	printf '\nYour code is affected by %s vulnerabilities from 1 module.\n' "$count"
}

CLEAN="=== Symbol Results ===

No vulnerabilities found.

Your code is affected by 0 vulnerabilities."

failures=0
# check <name> <wanted exit> <scanner status> <scanner output>
check() {
	got=0
	FAKE_STATUS=$3 FAKE_OUTPUT=$4 PATH="$work/bin:$PATH" sh "$here/govulncheck.sh" >"$work/log" 2>&1 || got=$?
	if [ "$got" -eq "$2" ]; then
		echo "ok   - $1"
		return
	fi
	echo "FAIL - $1: exit $got, wanted $2"
	sed 's/^/       /' "$work/log"
	failures=$((failures + 1))
}

check "a clean scan passes" "$PASS" 0 "$CLEAN"
check "only accepted advisories pass" "$PASS" "$SCANNER_FOUND" "$(report 1 "$ACCEPTED")"
check "an unaccepted advisory fails as policy" "$FAIL_UNACCEPTED" "$SCANNER_FOUND" "$(report 1 "$UNACCEPTED")"
check "one unaccepted among accepted fails as policy" "$FAIL_UNACCEPTED" "$SCANNER_FOUND" "$(report 2 "$ACCEPTED" "$UNACCEPTED")"

check "exit 42 with no advisory is not a pass" "$FAIL_INCOMPLETE" 42 "govulncheck: startup error"
check "a crash after printing an accepted ID is not a pass" "$FAIL_INCOMPLETE" 1 "$(report 1 "$ACCEPTED")"
check "exit 1 with no output is not a pass" "$FAIL_INCOMPLETE" 1 ""
check "exit 0 with no verdict is not a pass" "$FAIL_INCOMPLETE" 0 "=== Symbol Results ==="
check "exit 0 with no output is not a pass" "$FAIL_INCOMPLETE" 0 ""
check "a found status with no report is not a pass" "$FAIL_INCOMPLETE" "$SCANNER_FOUND" "garbage"
check "a report with no summary is cut short" "$FAIL_INCOMPLETE" "$SCANNER_FOUND" "$(report 1 "$ACCEPTED" | sed '$d')"
check "a report listing fewer than its summary is cut short" "$FAIL_INCOMPLETE" "$SCANNER_FOUND" "$(report 2 "$ACCEPTED")"
check "a found status claiming zero is not a pass" "$FAIL_INCOMPLETE" "$SCANNER_FOUND" "$(report 0)"

got=0
FAKE_BUILD_FAILS=1 FAKE_STATUS=0 FAKE_OUTPUT="$CLEAN" PATH="$work/bin:$PATH" sh "$here/govulncheck.sh" >"$work/log" 2>&1 || got=$?
if [ "$got" -eq "$FAIL_INCOMPLETE" ]; then
	echo "ok   - a scanner that will not build is not a pass"
else
	echo "FAIL - a scanner that will not build: exit $got, wanted $FAIL_INCOMPLETE"
	failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
	echo "✗ $failures govulncheck wrapper check(s) failed"
	exit 1
fi
echo "✓ govulncheck wrapper contract holds"
