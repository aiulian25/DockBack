#!/usr/bin/env bash
# End-to-end disaster-recovery drill (PLAN §6.11/§9.4): stand up an isolated
# DockBack stack, back up a throwaway container that holds a known sentinel
# file, TAMPER the volume, restore the backup, and assert the original data
# comes back — proving backup -> always-on verify -> restore actually works.
#
# Self-contained and safe to run anywhere with Docker: it uses its own compose
# project, a localhost-only port, and throwaway volumes/containers, all removed
# on exit. It only ever backs up the one throwaway container it creates.
#
# Two cases, selected with E2E_CASE:
#
#   sentinel (default) — the drill above: volume data lost and brought back.
#   pg-hash            — #18's content baseline: a PostgreSQL database with a
#                        timestamptz column is backed up, restored into another
#                        container, and the restored cluster is then put in a
#                        DIFFERENT TimeZone before being hashed again. R4 §31
#                        made seven false table mismatches out of eight that way;
#                        the hashes must come back identical.
#
#   ./scripts/e2e.sh
#   E2E_CASE=pg-hash ./scripts/e2e.sh
set -euo pipefail

cd "$(dirname "$0")/.."

E2E_CASE="${E2E_CASE:-sentinel}"
PORT="${E2E_PORT:-28999}"
PROJECT="dockback-e2e"
COMPOSE=(docker compose -p "$PROJECT" -f scripts/e2e.compose.yml)
SUFFIX="$(date +%s)-$$"
TARGET="dback-e2e-target-$SUFFIX"
VOL="dback-e2e-vol-$SUFFIX"
SENTINEL="ORIGINAL-$SUFFIX"
BASE="http://127.0.0.1:$PORT"
JAR="$(mktemp)"
export DOCKBACK_ENCRYPTION_KEY="$(openssl rand -hex 32)"
export DOCKBACK_ADMIN_USER="admin"
export DOCKBACK_ADMIN_PASSWORD="e2e-$(openssl rand -hex 8)"
export E2E_PORT="$PORT"

step() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mE2E FAILED: %s\033[0m\n' "$*" >&2; exit 1; }

# Containers a case creates register here so cleanup sweeps them all.
EXTRA_CONTAINERS=()

# #36: the image set as it stands BEFORE the run, so the teardown can prove
# DockBack put back whatever it borrowed.
#
# The fixtures' own images are pulled first and deliberately: this asserts what
# DOCKBACK left behind, not what the test itself asked the daemon to fetch.
# Compared by ID, never with `docker image ls --filter dangling=true` — removing
# an image by tag leaves it untagged but still carrying its repo digest, so it is
# not dangling and that filter reports a clean teardown over a multi-gigabyte
# orphan.
IMG_BEFORE="$(mktemp)"
snapshot_images() {
  for ref in alpine:latest alpine:3.20 postgres:16-alpine; do
    docker image inspect "$ref" >/dev/null 2>&1 || docker pull -q "$ref" >/dev/null 2>&1 || true
  done
  docker image ls -q --no-trunc | sort -u > "$IMG_BEFORE"
  echo "  image baseline: $(wc -l < "$IMG_BEFORE") images"
}

assert_no_leaked_images() {
  local after leaked
  after="$(mktemp)"
  docker image ls -q --no-trunc | sort -u > "$after"
  leaked="$(comm -13 "$IMG_BEFORE" "$after")"
  rm -f "$after"
  if [ -n "$leaked" ]; then
    printf '\033[1;31m  images left behind by the run:\033[0m\n%s\n' "$leaked" >&2
    printf '  (each is still present by ID; `docker image prune` will NOT reclaim one that kept a repo digest)\n' >&2
    return 1
  fi
  echo "  no images left behind — every image the run pulled was given back"
}

cleanup() {
  step "Cleanup"
  docker rm -f "$TARGET" >/dev/null 2>&1 || true
  for c in ${EXTRA_CONTAINERS+"${EXTRA_CONTAINERS[@]}"}; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  docker volume rm "$VOL" >/dev/null 2>&1 || true
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  # #36: last, after every container this run made is gone — an image is not
  # removable while something built on it still exists.
  assert_no_leaked_images || fail "the run left images on the host"
  rm -f "$JAR" "$IMG_BEFORE"
}
trap cleanup EXIT

# jget <json-path-python> — read stdin JSON and print an expression of `d`.
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }

step "Build + start isolated stack on $BASE"
"${COMPOSE[@]}" up -d --build

step "Wait for /healthz"
for i in $(seq 1 60); do
  curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break
  [ "$i" = 60 ] && fail "app never became healthy"
  sleep 1
done

step "Log in"
CSRF="$(curl -fsS -c "$JAR" -X POST "$BASE/api/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$DOCKBACK_ADMIN_USER\",\"password\":\"$DOCKBACK_ADMIN_PASSWORD\"}" | jget 'd["csrf"]')"
[ -n "$CSRF" ] || fail "login failed"
auth=(-b "$JAR" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json')

# ── shared helpers ───────────────────────────────────────────────────────────

# find_container <name> — the app's id for a container, once it has seen it.
find_container() {
  local name="$1" cid="" i
  for i in $(seq 1 30); do
    cid="$(curl -fsS -b "$JAR" "$BASE/api/nodes/local/containers?q=$name&page_size=5" \
      | jget 'd["containers"][0]["id"] if d["containers"] else ""' 2>/dev/null || true)"
    [ -n "$cid" ] && break
    sleep 2
  done
  [ -n "$cid" ] || fail "container $name not discovered by the app"
  printf '%s' "$cid"
}

# backup_and_verify <container-id> <target-name> — back up, wait for verified,
# print the backup id.
backup_and_verify() {
  local cid="$1" name="$2" row i
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups" \
    -d "{\"node_id\":\"local\",\"container_id\":\"$cid\",\"compression\":\"balanced\",\"destinations\":[]}" >/dev/null
  for i in $(seq 1 60); do
    row="$(curl -fsS -b "$JAR" "$BASE/api/backups?node_id=local&page_size=20" \
      | jget '([b for b in d["items"] if b["target_name"]=="'"$name"'"] or [{}])[0].get("status","")+"/"+([b for b in d["items"] if b["target_name"]=="'"$name"'"] or [{}])[0].get("verified","")')"
    echo "  [$i] $row" >&2
    case "$row" in
      success/verified) break ;;
      failed/*) fail "backup of $name failed" ;;
    esac
    [ "$i" = 60 ] && fail "backup of $name did not verify in time"
    sleep 3
  done
  curl -fsS -b "$JAR" "$BASE/api/backups?node_id=local&page_size=20" \
    | jget '[b for b in d["items"] if b["target_name"]=="'"$name"'"][0]["id"]'
}

# table_hashes <backup-id> — the manifest's per-table content baseline, as
# "table hash" lines sorted by table.
table_hashes() {
  curl -fsS -b "$JAR" "$BASE/api/backups/$1" \
    | jget 'chr(10).join(sorted(k+" "+v for db in json.loads(d["manifest_json"]).get("databases",[]) for k,v in (db.get("table_hashes") or {}).items()))'
}

# ── case: sentinel ───────────────────────────────────────────────────────────

case_sentinel() {
  step "Create throwaway target container with sentinel '$SENTINEL'"
  docker volume create "$VOL" >/dev/null
  docker run -d --name "$TARGET" -v "$VOL":/data alpine sh -c "echo '$SENTINEL' > /data/sentinel; sleep 600" >/dev/null
  readvol() { docker run --rm -v "$VOL":/d:ro alpine cat /d/sentinel 2>/dev/null | tr -d '[:space:]'; }
  [ "$(readvol)" = "$SENTINEL" ] || fail "sentinel not written"

  step "Find the target container on the local node"
  local CID BID
  CID="$(find_container "$TARGET")"
  echo "  container id: ${CID:0:12}"

  step "Back it up (local destination) and wait for verified"
  BID="$(backup_and_verify "$CID" "$TARGET")"
  echo "  backup id: $BID"

  step "Recover the archive WITHOUT DockBack (F35: dockback-recover.py on the host)"
  E2E_PROJECT="$PROJECT" E2E_COMPOSE_FILE="scripts/e2e.compose.yml" \
    bash scripts/test-recover.sh "$TARGET" || fail "offline recovery (dockback-recover.py) failed"

  step "TAMPER the volume — SAME SIZE, different bytes (R5 §9.3's defeat of manifests)"
  # Every character is replaced and the length is unchanged, so a path+size
  # listing sees nothing at all. R5 §9.3: six fixed-width markers rewritten by a
  # clone produced an identical manifest and a different tree — "the check that
  # passed perfectly is the one that could not see the change".
  local TAMPERED
  TAMPERED="$(printf '%s' "$SENTINEL" | tr 'A-Za-z0-9-' 'X')"
  docker run --rm -v "$VOL":/d alpine sh -c "printf '%s\n' '$TAMPERED' > /d/sentinel"
  [ "$(readvol)" = "$TAMPERED" ] || fail "tamper failed"
  [ "${#TAMPERED}" = "${#SENTINEL}" ] || fail "the tamper changed the size, which is not the case being tested"
  echo "  tampered in place: ${#SENTINEL} bytes before and after"

  step "Restore the backup into the target"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"database\":false,\"confirm\":true}" >/dev/null

  step "Wait for the original sentinel to return"
  ok=""
  for i in $(seq 1 60); do
    cur="$(readvol || true)"
    echo "  [$i] sentinel=$cur"
    if [ "$cur" = "$SENTINEL" ]; then ok=1; break; fi
    sleep 3
  done
  [ -n "$ok" ] || fail "restore did not bring back the original data (got '$(readvol)')"

  # #1: the empty-data guard refuses to start a container whose restored mounts are
  # empty where the backup recorded data. This run round-tripped its data, so the
  # guard must have stayed quiet — a guard that fires on a good restore is an outage
  # of its own, and that regression is invisible without asserting the absence.
  step "Assert the restart-policy finding fired"
  # #8: this container is created with no restart policy at all, so it would not
  # come back after a reboot. The capture must say so — and must NOT change it.
  local CAPLOG0
  CAPLOG0="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" || true)"
  case "$CAPLOG0" in
    *"does not start it when the Docker daemon starts"*) echo "  restart policy reported, not rewritten" ;;
    *) fail "a container with no restart policy must produce the #8 finding" ;;
  esac
  POLICY="$(docker inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$TARGET" 2>/dev/null || true)"
  [ "$POLICY" = "no" ] || [ -z "$POLICY" ] || fail "the capture must never change the restart policy (now '$POLICY')"

  step "Assert the during-copy drift report ran"
  # #25: this container is quiesced with a pause, not a stop, so the drift walk
  # runs and must find nothing moved underneath the archive.
  local CAPLOG
  CAPLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" || true)"
  case "$CAPLOG" in
    *"Nothing changed underneath the copy"*) echo "  nothing drifted during the copy" ;;
    *"changed while the archive was being written"*) echo "  drift reported (the container wrote during the copy)" ;;
    *) fail "the during-copy drift report never ran" ;;
  esac

  step "Assert the file content check ran and passed pre-start"
  # #41: the index carries a content hash per file, and the restore checks them
  # back before the container starts. A same-size tamper is exactly what size and
  # mtime cannot see, so this is the check that has to notice it was repaired.
  local FILELOG
  FILELOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" || true)"
  case "$FILELOG" in
    *"came back with DIFFERENT CONTENT"*) fail "the file check reported a mismatch on a byte-perfect restore" ;;
    *"Files verified before"*)
      printf '%s\n' "$FILELOG" \
        | python3 -c "import sys,json;print('\n'.join('    '+l['msg'] for l in json.load(sys.stdin).get('lines',[]) if 'Files verified before' in l['msg'] or 'Restored mount ownership' in l['msg']))" ;;
    *) printf '%s\n' "$FILELOG" | python3 -c "import sys,json;print(chr(10).join('    '+l['level']+' '+l['msg'] for l in json.load(sys.stdin).get('lines',[])[-40:]))" >&2
       fail "the restore never checked the restored files against the backup's content hashes" ;;
  esac

  step "Assert the writability test reached a verdict"
  # #34: it must run as the application's own uid, and say so honestly when the
  # image declares none — never pass by testing as root.
  case "$FILELOG" in
    *"Writable as uid"*)           echo "  writable as the application's own uid" ;;
    *"cannot write to"*)           echo "  not writable — reported with the chown to fix it" ;;
    *"Writability not tested"*)    echo "  not tested, and said so rather than passing as root" ;;
    *) fail "the writability test never reached a verdict" ;;
  esac

  step "Assert the empty-data guard did NOT refuse this healthy restore"
  RUNLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" || true)"
  case "$RUNLOG" in
    *"hold no data"*) fail "the empty-data guard refused a restore that round-tripped correctly" ;;
  esac
  echo "  clean: no empty-data refusal in the run log"

  printf '\n\033[1;32mE2E PASSED (sentinel): backup -> verify -> restore round-tripped the data.\033[0m\n'
}

# ── case: pg-hash ────────────────────────────────────────────────────────────
#
# #18's content baseline, and R4 §31's rule for computing it.
#
# The source cluster has no TZ. After the restore the target's cluster is put in
# Europe/London deliberately — the exact configuration that made seven false
# table mismatches out of eight — and hashed again. Identical hashes are the
# whole claim: the baseline proves CONTENT came back, and says nothing about the
# timezone either side happened to be in.

case_pg_hash() {
  local SRC="dback-e2e-pgsrc-$SUFFIX" DST="dback-e2e-pgdst-$SUFFIX"
  local SRC_CID DST_CID SRC_BID DST_BID SRC_HASHES DST_HASHES i
  EXTRA_CONTAINERS+=("$SRC" "$DST")

  local seed="create table docs(id serial primary key, title text, added timestamptz, modified timestamp, due date, price money);
insert into docs(title,added,modified,due,price) values
 ('alpha','2024-03-01 10:00:00+00','2024-03-01 10:00:00','2024-03-05',12.50),
 ('beta','2024-07-15 23:30:00+00','2024-07-15 23:30:00','2024-07-20',99.99);
create table keyless(a text, b int);
insert into keyless values ('x',1),('y',2);"

  step "Stand up a source PostgreSQL (no TZ) and seed it"
  docker run -d --name "$SRC" -e POSTGRES_PASSWORD=e2e postgres:16-alpine >/dev/null
  for i in $(seq 1 60); do docker exec "$SRC" pg_isready -q 2>/dev/null && break; [ "$i" = 60 ] && fail "source postgres never became ready"; sleep 1; done
  docker exec -i "$SRC" psql -U postgres -q -v ON_ERROR_STOP=1 -c "$seed" >/dev/null || fail "seeding the source failed"
  echo "  source TimeZone: $(docker exec "$SRC" psql -U postgres -Atqc 'show TimeZone')"

  step "Back up the source and wait for verified"
  SRC_CID="$(find_container "$SRC")"
  SRC_BID="$(backup_and_verify "$SRC_CID" "$SRC")"
  echo "  backup id: $SRC_BID"

  step "Assert the manifest carries a content baseline per table"
  SRC_HASHES="$(table_hashes "$SRC_BID")"
  [ -n "$SRC_HASHES" ] || fail "the manifest carries no table_hashes — the content baseline did not run"
  printf '%s\n' "$SRC_HASHES" | sed 's/^/    /'
  printf '%s\n' "$SRC_HASHES" | grep -q 'public\.docs ' || fail "no baseline for the timestamptz table"
  printf '%s\n' "$SRC_HASHES" | grep -q 'public\.keyless ' || fail "a table without a primary key must still be hashed"

  step "Assert the missing-healthcheck finding fired, and the capture changed nothing"
  # #16: postgres:16-alpine ships no HEALTHCHECK and this container declares none,
  # so anything using `depends_on: service_started` would start against a server
  # that cannot yet answer. The capture must say so — and must leave the container
  # exactly as it found it, because injection is the operator's choice at restore.
  local PGCAPLOG
  PGCAPLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$SRC_BID/log" || true)"
  case "$PGCAPLOG" in
    *"defines no healthcheck"*) echo "  missing healthcheck reported with the probe it would add" ;;
    *) fail "a database with no healthcheck must produce the #16 finding" ;;
  esac
  printf '%s\n' "$PGCAPLOG" | grep -q 'select 1' || fail "the finding must name the probe it offers, not just the absence"
  SRCPROBE="$(docker inspect -f '{{json .Config.Healthcheck}}' "$SRC" 2>/dev/null || true)"
  [ "$SRCPROBE" = "null" ] || fail "the capture must never write a healthcheck into the source (now '$SRCPROBE')"

  step "Stand up a target PostgreSQL and restore into it"
  docker run -d --name "$DST" -e POSTGRES_PASSWORD=e2e postgres:16-alpine >/dev/null
  for i in $(seq 1 60); do docker exec "$DST" pg_isready -q 2>/dev/null && break; [ "$i" = 60 ] && fail "target postgres never became ready"; sleep 1; done
  DST_CID="$(find_container "$DST")"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$SRC_BID/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$DST_CID\",\"volumes\":false,\"database\":true,\"confirm\":true,\"confirm_incompatible\":true}" >/dev/null

  step "Wait for the restored rows to appear"
  for i in $(seq 1 60); do
    if [ "$(docker exec "$DST" psql -U postgres -Atqc 'select count(*) from docs' 2>/dev/null || true)" = "2" ]; then break; fi
    echo "  [$i] waiting for the restored table"
    [ "$i" = 60 ] && fail "the restore never landed the rows"
    sleep 3
  done

  step "Assert the restore checked the content baseline back, pre-start"
  # #18/#26: the same pass runs against the restored database at the only clean
  # comparison point — after the import, before the application (re)starts — and
  # states the register's bar. This restore is byte-perfect, so every durable
  # table must be reported identical.
  local RESTORELOG
  RESTORELOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$SRC_BID/log" || true)"
  # The register's wording bar (R3 #26): "N/N durable identical", with any
  # volatile drift reported as its own clause rather than folded into the total.
  case "$RESTORELOG" in
    *"durable identical"*) ;;
    *"came back with different content"*) fail "the restore reported a content mismatch on a byte-perfect restore" ;;
    *) fail "the restore never checked the content baseline back" ;;
  esac
  printf '%s\n' "$RESTORELOG" \
    | python3 -c "import sys,json;print('\n'.join('    '+l['msg'] for l in json.load(sys.stdin).get('lines',[]) if 'durable identical' in l['msg']))"
  case "$RESTORELOG" in
    *"Content verified before"*) echo "  verdict is labelled pre-start" ;;
    *) fail "the verdict is not the pre-start one" ;;
  esac

  step "Put the RESTORED cluster in Europe/London — R4 §31's exact configuration"
  docker exec "$DST" psql -U postgres -q -c "ALTER SYSTEM SET TimeZone = 'Europe/London'" >/dev/null
  docker exec "$DST" psql -U postgres -q -c "SELECT pg_reload_conf()" >/dev/null
  local dsttz
  dsttz="$(docker exec "$DST" psql -U postgres -Atqc 'show TimeZone')"
  echo "  restored TimeZone: $dsttz"
  [ "$dsttz" = "Europe/London" ] || fail "the target cluster is not in a different timezone, so this proves nothing"

  step "Back up the restored container and compare the baselines"
  DST_BID="$(backup_and_verify "$DST_CID" "$DST")"
  DST_HASHES="$(table_hashes "$DST_BID")"
  [ -n "$DST_HASHES" ] || fail "the restored backup carries no table_hashes"
  printf '%s\n' "$DST_HASHES" | sed 's/^/    /'

  # The manifest keys are <database>.<schema>.<table>; both sides are the
  # `postgres` database here, so the keys line up and only the hashes matter.
  if [ "$SRC_HASHES" != "$DST_HASHES" ]; then
    printf '\n  source:\n%s\n\n  restored (Europe/London):\n%s\n' "$SRC_HASHES" "$DST_HASHES" >&2
    fail "identical data hashed differently across timezones — this is R4 §31 all over again"
  fi
  echo "  identical across UTC and Europe/London"

  printf '\n\033[1;32mE2E PASSED (pg-hash): the content baseline is the same data, whatever timezone reads it.\033[0m\n'
}

step "Record the image set, so the teardown can prove nothing was left behind"
snapshot_images

case "$E2E_CASE" in
  sentinel) case_sentinel ;;
  pg-hash)  case_pg_hash ;;
  *) fail "unknown E2E_CASE '$E2E_CASE' (known: sentinel, pg-hash)" ;;
esac
