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
# Twenty cases, selected with E2E_CASE:
#
#   sentinel (default) — the drill above: volume data lost and brought back.
#   deleted-binds      — the recovery of 2026-10-04: a RUNNING container whose
#                        folder and secret-file binds are read-only has its whole
#                        host folder deleted, then is restored IN PLACE. Both must
#                        come back — the secret as a real file, mode 600, not the
#                        empty directory Docker invents for a missing file bind.
#   moved-bind         — a secret-file bind moved by a redeploy after the backup,
#                        then an in-place restore: the file must land where the
#                        container mounts it now, and its old path must stay
#                        unwritten.
#   stack-folder       — the rest of that night: a Compose project's folder is
#                        deleted while it runs, then the stack is restored with
#                        "rebuild stack folder" on. Your own compose file and .env
#                        must come back as the files Compose runs, the .env with
#                        no duplicated key, and `docker compose up -d` from the
#                        restored folder must find nothing to change.
#   files-only         — the restore that night needed: a running stack's folder
#                        is deleted, and a files-only stack restore puts back the
#                        compose file, .env, project files and the secret-file
#                        bind without stopping or restarting anything, and names
#                        the data folder only a full restore can bring back.
#   stack-selection    — a restore that names one of a stack's two services and a
#                        copy to read from. The named service gets its data back
#                        from that copy alone; the other is not stopped, restarted
#                        or written to. With that copy corrupted the restore still
#                        works and warns that it fell back. A name the stack does
#                        not have changes nothing.
#   clone-ownership    — a copy asked to use a name another container holds is
#                        refused and that container is unchanged; a free name
#                        works; a standby rehearsal beside a container called
#                        <target>-standby passes, leaves that container alone and
#                        removes only its own clone.
#   clone-sandbox      — a copy of a container holding a device, an extra capability
#                        and the host's PID namespace gets none of them and no
#                        network, the log names what it lost, and a PostgreSQL copy
#                        still gets its data back with no network.
#   revert-rollback    — a revert to an older image whose backup cannot pass the
#                        health gate. The rollback must bring back the image, the
#                        configuration and the data the container had before.
#   shutdown-holds     — DockBack is stopped while it holds a container paused. On
#                        SIGTERM it lets the container go before exiting; after
#                        SIGKILL the restarted instance does. A container somebody
#                        else paused is left paused throughout.
#   hook-cleanup       — a pre-hook that changes state and then fails still gets its
#                        post-hook; a good run ends its window before compressing;
#                        DockBack killed inside a window runs the post-hook on return.
#   hold-required      — the socket proxy refuses to stop containers. A backup that
#                        asked for a stop fails rather than copy live and call it
#                        consistent, and a stack is not reported as one point in
#                        time. Pause and live modes still work, and say which they were.
#   incremental-fidelity — a full then a delta, across a chmod, a chown, a same-size
#                        write under a preserved mtime, a changed link, an empty
#                        directory and both type changes, restored and compared.
#   offsite-envelope   — an offsite copy loses the manifest beside its archive. A
#                        scrub finds it, Send offsite repairs it without sending
#                        the archive again, and the offline tool then opens it from
#                        the destination's files alone, which it could not before.
#   offsite-window     — a copy deferred by a closed upload window stays deferred
#                        across a restart, then goes by itself once the window
#                        opens, with no new backup and no one asking, and only once.
#   restore-outcome    — a post-restore hook logs an error and the restore still
#                        succeeds, and the record says so; a repeat restore keeps its
#                        own record; a restore cut off by SIGKILL reads as interrupted.
#   clone-redaction    — a copy of a Plex-profile image, which is a tagged alpine and
#                        holds invented values, comes up with its account token gone
#                        before its first start, and the original keeps its own.
#   stack-parity       — a service's saved choices reach its app-consistent stack
#                        snapshot: its image is bundled, its file index written, the
#                        paths its profile never keeps left out, and what a group
#                        cannot honour (export, incremental) recorded rather than dropped.
#   delete-incomplete  — a backup is deleted while its offsite copy cannot be
#                        removed. The local copy goes, the backup stays listed as
#                        being deleted and names the copy that is left, it can no
#                        longer be restored, and deleting it again once the
#                        destination allows it removes the copy and the record.
#   pg-hash            — #18's content baseline: a PostgreSQL database with a
#                        timestamptz column is backed up, restored into another
#                        container, and the restored cluster is then put in a
#                        DIFFERENT TimeZone before being hashed again. R4 §31
#                        made seven false table mismatches out of eight that way;
#                        the hashes must come back identical.
#
#   ./scripts/e2e.sh
#   E2E_CASE=pg-hash ./scripts/e2e.sh
#   E2E_CASE=offsite-envelope ./scripts/e2e.sh
#   E2E_CASE=delete-incomplete ./scripts/e2e.sh
#   E2E_CASE=offsite-window ./scripts/e2e.sh
#   E2E_CASE=restore-outcome ./scripts/e2e.sh
#   E2E_CASE=clone-redaction ./scripts/e2e.sh
#   E2E_CASE=stack-parity ./scripts/e2e.sh
#   E2E_CASE=deleted-binds ./scripts/e2e.sh
#   E2E_CASE=stack-folder ./scripts/e2e.sh
#   E2E_CASE=files-only ./scripts/e2e.sh
#   E2E_CASE=stack-selection ./scripts/e2e.sh
#   E2E_CASE=clone-ownership ./scripts/e2e.sh
#   E2E_CASE=clone-sandbox ./scripts/e2e.sh
#   E2E_CASE=revert-rollback ./scripts/e2e.sh
#   E2E_CASE=shutdown-holds ./scripts/e2e.sh
#   E2E_CASE=hook-cleanup ./scripts/e2e.sh
#   E2E_CASE=hold-required ./scripts/e2e.sh
#   E2E_CASE=incremental-fidelity ./scripts/e2e.sh
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

# Containers, Compose projects and networks a case creates register here so
# cleanup sweeps them all.
EXTRA_CONTAINERS=()
EXTRA_PROJECTS=()
EXTRA_NETWORKS=()

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
  for p in ${EXTRA_PROJECTS+"${EXTRA_PROJECTS[@]}"}; do
    docker ps -aq --filter "label=com.docker.compose.project=$p" | xargs -r docker rm -f >/dev/null 2>&1 || true
    docker volume ls -q --filter "label=com.docker.compose.project=$p" | xargs -r docker volume rm >/dev/null 2>&1 || true
    docker network ls -q --filter "label=com.docker.compose.project=$p" | xargs -r docker network rm >/dev/null 2>&1 || true
  done
  for n in ${EXTRA_NETWORKS+"${EXTRA_NETWORKS[@]}"}; do docker network rm "$n" >/dev/null 2>&1 || true; done
  docker volume rm "$VOL" >/dev/null 2>&1 || true
  if [ -n "${HOSTDIR:-}" ]; then
    docker run --rm -v "$(dirname "$HOSTDIR")":/p alpine rm -rf "/p/$(basename "$HOSTDIR")" >/dev/null 2>&1 || true
  fi
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  # #36: last, after every container this run made is gone — an image is not
  # removable while something built on it still exists.
  assert_no_leaked_images || fail "the run left images on the host"
  rm -f "$JAR" "$IMG_BEFORE"
  [ -z "$PROXY_OVERRIDE_DIR" ] || rm -rf "$PROXY_OVERRIDE_DIR"
}
trap cleanup EXIT

# jget <json-path-python> — read stdin JSON and print an expression of `d`.
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }

# hold-required: a proxy that refuses to stop containers, the real permission failure.
# Its ALLOW_STOP setting cannot do it: that only adds an allow, and CONTAINERS allows
# stop anyway. So a deny goes ahead of every allow in the proxy's own config.
PROXY_OVERRIDE_DIR=""
if [ "$E2E_CASE" = "hold-required" ]; then
  PROXY_OVERRIDE_DIR="$(mktemp -d)"
  PROXY_TEMPLATE="/usr/local/etc/haproxy/haproxy.cfg.template"
  PROXY_IMAGE="$(sed -n 's/^ *image: \(tecnativa.*\)$/\1/p' scripts/e2e.compose.yml)"
  docker run --rm --entrypoint cat "$PROXY_IMAGE" "$PROXY_TEMPLATE" \
    | sed '/^frontend dockerfrontend/,/bind /{/bind /a\    http-request deny if { path,url_dec -m reg -i ^(/v[\\d\\.]+)?/containers/[a-zA-Z0-9_.-]+/stop }
}' > "$PROXY_OVERRIDE_DIR/haproxy.cfg.template"
  grep -q 'http-request deny if' "$PROXY_OVERRIDE_DIR/haproxy.cfg.template" || fail "could not add the deny rule"
  printf 'services:\n  socket-proxy:\n    volumes:\n      - %s:%s:ro\n' "$PROXY_OVERRIDE_DIR/haproxy.cfg.template" "$PROXY_TEMPLATE" > "$PROXY_OVERRIDE_DIR/override.yml"
  COMPOSE+=(-f "$PROXY_OVERRIDE_DIR/override.yml")
fi

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
# Saving commands that run inside a container (hooks, an export profile) asks for the
# password again, so those requests carry it as this JSON member.
CONFIRM_PASSWORD="\"password\":\"$DOCKBACK_ADMIN_PASSWORD\""

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

# backup_and_verify <container-id> <target-name> [destination-id] — back up, wait
# for verified, print the backup id.
backup_and_verify() {
  local cid="$1" name="$2" dest="${3:-}" row i
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups" \
    -d "{\"node_id\":\"local\",\"container_id\":\"$cid\",\"compression\":\"balanced\",\"destinations\":[${dest:+\"$dest\"}]}" >/dev/null
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

# log_in — a restart signs everybody out.
log_in() {
  local i
  for i in $(seq 1 60); do
    curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break
    [ "$i" = 60 ] && fail "the app never came back"
    sleep 1
  done
  CSRF="$(curl -fsS -c "$JAR" -X POST "$BASE/api/login" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$DOCKBACK_ADMIN_USER\",\"password\":\"$DOCKBACK_ADMIN_PASSWORD\"}" | jget 'd["csrf"]')"
  auth=(-b "$JAR" -H "X-CSRF-Token: $CSRF" -H 'Content-Type: application/json')
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

  step "Assert the short freeze ran"
  # This container is quiesced with a pause, so its volumes are copied while it
  # runs and it is held only to copy again what changed meanwhile — which also
  # leaves the #25 drift report nothing to say, so it does not run.
  local CAPLOG
  CAPLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" || true)"
  case "$CAPLOG" in
    *"Nothing changed during the live copy"*) echo "  nothing changed during the live copy" ;;
    *"that changed during the live copy"*) echo "  what changed during the live copy was copied again while held" ;;
    *) fail "the short freeze never ran" ;;
  esac
  case "$CAPLOG" in
    *"it was paused for"*) echo "  the log says how long the container was held" ;;
    *) fail "the log must say how long the container was held" ;;
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

# ── case: deleted-binds ──────────────────────────────────────────────────────

case_deleted_binds() {
  HOSTDIR="$(mktemp -d /tmp/dback-e2e-binds-XXXXXX)"
  local APP="$HOSTDIR/app" KEY_CONTENT="vapid-$SUFFIX" CONF_CONTENT="setting=$SUFFIX"
  mkdir -p "$APP/config"
  printf '%s\n' "$CONF_CONTENT" > "$APP/config/settings.ini"
  printf '%s' "$KEY_CONTENT" > "$APP/secret.key"
  chmod 600 "$APP/secret.key"

  step "Create a running container with READ-ONLY folder and secret-file binds"
  docker run -d --name "$TARGET" \
    -v "$APP/config":/config:ro -v "$APP/secret.key":/run/secrets/app.key:ro \
    alpine sleep 600 >/dev/null

  step "Find it and back it up"
  local CID BID
  CID="$(find_container "$TARGET")"
  BID="$(backup_and_verify "$CID" "$TARGET")"
  echo "  backup id: $BID"

  step "The disaster: delete the whole host folder while the container keeps running"
  rm -rf "$APP"
  [ ! -e "$APP" ] || fail "could not delete the host folder"
  docker inspect -f '{{.State.Running}}' "$TARGET" | grep -q true || fail "the container should still be running"

  step "Restore IN PLACE (container exists, not recreated), with the safety snapshot"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"database\":false,\"snapshot\":true,\"confirm\":true}" >/dev/null

  step "Wait for the restore to finish"
  local RLOG="" i finished=""
  for i in $(seq 1 100); do
    RLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" || true)"
    case "$RLOG" in
      *"Restored container is healthy"*) finished=1; break ;;
      *"Restore failed"*|*"Restore refused"*|*"rolling back"*|*"did not become healthy"*)
        printf '%s\n' "$RLOG" | python3 -c "import sys,json;print(chr(10).join('    '+l['level']+' '+l['msg'] for l in json.load(sys.stdin).get('lines',[])[-25:]))" >&2
        fail "the in-place restore did not succeed" ;;
    esac
    echo "  [$i] restoring"
    sleep 3
  done
  [ -n "$finished" ] || fail "the restore did not finish in time"

  step "Assert both binds came back as they were"
  [ ! -d "$APP/secret.key" ] || fail "secret.key came back as an empty DIRECTORY — the 2026-10-04 failure"
  [ -f "$APP/secret.key" ] || fail "secret.key did not come back"
  [ -f "$APP/config/settings.ini" ] || fail "the config folder did not come back"
  [ "$(stat -c %u:%g "$APP/secret.key")" = "$(id -u):$(id -g)" ] || fail "secret.key came back owned by $(stat -c %u:%g "$APP/secret.key"), not $(id -u):$(id -g)"
  [ "$(cat "$APP/secret.key")" = "$KEY_CONTENT" ] || fail "secret.key came back with the wrong content"
  [ "$(stat -c %a "$APP/secret.key")" = "600" ] || fail "secret.key came back as mode $(stat -c %a "$APP/secret.key"), not 600"
  [ "$(tr -d '\n' < "$APP/config/settings.ini")" = "$CONF_CONTENT" ] || fail "the config folder came back with the wrong content"
  docker inspect -f '{{range .Mounts}}{{.RW}} {{end}}' "$TARGET" | grep -q true && fail "a read-only mount became writable"
  printf '\033[1;32mE2E PASSED (deleted-binds): a deleted host folder came back in place — folder and secret file, owner and mode intact.\033[0m\n'
}

# ── case: moved-bind ─────────────────────────────────────────────────────────

case_moved_bind() {
  HOSTDIR="$(mktemp -d /tmp/dback-e2e-moved-XXXXXX)"
  local APP="$HOSTDIR/app" KEY_CONTENT="key-$SUFFIX"
  mkdir -p "$APP"
  printf '%s' "$KEY_CONTENT" > "$APP/secret.key"
  chmod 600 "$APP/secret.key"

  step "Create a running container with a secret-file bind, and back it up"
  docker run -d --name "$TARGET" -v "$APP/secret.key":/run/secrets/app.key:ro alpine sleep 600 >/dev/null
  local CID BID
  CID="$(find_container "$TARGET")"
  BID="$(backup_and_verify "$CID" "$TARGET")"
  echo "  backup id: $BID"

  step "Redeploy it with the secret moved into a secrets folder, and the file there damaged"
  docker rm -f "$TARGET" >/dev/null
  mkdir -p "$APP/secrets"
  mv "$APP/secret.key" "$APP/secrets/secret.key"
  printf 'damaged' > "$APP/secrets/secret.key"
  docker run -d --name "$TARGET" -v "$APP/secrets/secret.key":/run/secrets/app.key:ro alpine sleep 600 >/dev/null
  CID="$(find_container "$TARGET")"

  step "Restore IN PLACE into the redeployed container"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"database\":false,\"snapshot\":true,\"confirm\":true}" >/dev/null

  step "Wait for the restore to finish"
  local RLOG="" i finished=""
  for i in $(seq 1 100); do
    RLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" || true)"
    case "$RLOG" in
      *"Restored container is healthy"*) finished=1; break ;;
      *"Restore failed"*|*"Restore refused"*|*"rolling back"*|*"did not become healthy"*)
        printf '%s\n' "$RLOG" | python3 -c "import sys,json;print(chr(10).join('    '+l['level']+' '+l['msg'] for l in json.load(sys.stdin).get('lines',[])[-25:]))" >&2
        fail "the in-place restore did not succeed" ;;
    esac
    echo "  [$i] restoring"
    sleep 3
  done
  [ -n "$finished" ] || fail "the restore did not finish in time"

  step "Assert the secret landed where the container reads it, and nowhere else"
  [ "$(cat "$APP/secrets/secret.key")" = "$KEY_CONTENT" ] || fail "the file the container mounts was not restored: $(cat "$APP/secrets/secret.key")"
  [ ! -e "$APP/secret.key" ] || fail "the backup's old path was written instead"
  [ "$(docker exec "$TARGET" cat /run/secrets/app.key)" = "$KEY_CONTENT" ] || fail "the running container does not see the restored secret"
  printf '\033[1;32mE2E PASSED (moved-bind): a moved secret-file bind was restored where the container mounts it now, and its old path was left alone.\033[0m\n'
}

# ── case: stack-folder ───────────────────────────────────────────────────────

case_stack_folder() {
  HOSTDIR="$(mktemp -d /tmp/dback-e2e-stack-XXXXXX)"
  local PROJ="dback-e2e-stack-$SUFFIX" NET="dback-e2e-shared-$SUFFIX"
  local DIR="$HOSTDIR/$PROJ"
  EXTRA_PROJECTS+=("$PROJ")
  EXTRA_NETWORKS+=("$NET")
  local compose_in_dir=(docker compose -p "$PROJ" --project-directory "$DIR" -f "$DIR/docker-compose.yml")

  step "Stand up a Compose project: shared network, named volume, a literal \$, an exported .env"
  mkdir -p "$DIR/config"
  printf 'setting=%s\n' "$SUFFIX" > "$DIR/config/app.ini"
  cat > "$DIR/docker-compose.yml" <<EOF
# Only the operator's own file has this comment.
services:
  app:
    image: alpine
    command: ["sleep", "600"]
    environment:
      TOKEN: \${TOKEN}
      HASH: "\$\$apr1\$\$e2e"
    volumes:
      - data:/data
      - ./config:/config
    networks: [shared, default]
volumes:
  data:
networks:
  shared:
    external: true
    name: $NET
EOF
  printf 'export TOKEN=token-%s\n' "$SUFFIX" > "$DIR/.env"
  mkdir -p "$DIR/scripts" "$DIR/node_modules/pkg"
  printf '#!/bin/sh\necho nightly %s\n' "$SUFFIX" > "$DIR/scripts/nightly.sh"
  chmod 755 "$DIR/scripts/nightly.sh"
  printf 'How this stack is run.\n' > "$DIR/README.md"
  printf 'rebuilt by npm\n' > "$DIR/node_modules/pkg/index.js"
  printf 'noise\n' > "$DIR/debug.log"
  printf '*.log\n' > "$DIR/.dockbackignore"
  cp "$DIR/docker-compose.yml" "$HOSTDIR/original-compose.yml"
  cp "$DIR/.env" "$HOSTDIR/original.env"
  docker network create "$NET" >/dev/null
  "${compose_in_dir[@]}" up -d --quiet-pull >/dev/null 2>&1 || fail "the test stack did not come up"

  step "Back it up"
  local CID BID BEFORE
  CID="$(find_container "$PROJ-app-1")"
  BID="$(backup_and_verify "$CID" "$PROJ-app-1")"
  echo "  backup id: $BID"
  BEFORE="$(docker ps -q --no-trunc --filter "label=com.docker.compose.project=$PROJ")"

  step "Download the whole stack in one file (step 25)"
  local TICKET ZIP="$HOSTDIR/stack.zip"
  TICKET="$(curl -fsS "${auth[@]}" -X POST "$BASE/api/nodes/local/stacks/$PROJ/export-grant" \
    -d "{\"password\":\"$DOCKBACK_ADMIN_PASSWORD\"}" | jget 'd["ticket"]')"
  [ -n "$TICKET" ] || fail "no download ticket for the stack"
  curl -fsS -b "$JAR" -o "$ZIP" "$BASE/api/nodes/local/stacks/$PROJ/download?ticket=$TICKET" || fail "the stack download failed"
  python3 -c '
import io, sys, tarfile, zipfile
z = zipfile.ZipFile(sys.argv[1])
names = z.namelist()
assert len(names) == 1 and names[0].startswith("app-") and names[0].endswith(".tar"), names
inner = tarfile.open(fileobj=io.BytesIO(z.read(names[0])))
members = inner.getnames()
assert "config/inspect.json" in members, members
assert "config/project-folder.tar" in members, members
print("  one zip: %s, holding the decrypted backup with its project folder" % names[0])
' "$ZIP" || fail "the stack download is not one zip of the decrypted backups"
  curl -fsS -b "$JAR" -o /dev/null "$BASE/api/nodes/local/stacks/$PROJ/download?ticket=$TICKET" && fail "a download ticket must work once"

  step "The disaster: delete the stack folder while the stack keeps running"
  rm -rf "$DIR"
  [ ! -e "$DIR" ] || fail "could not delete the stack folder"

  step "Restore the stack with the stack folder rebuilt"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/nodes/local/stacks/$PROJ/restore?reconstruct_host=true" >/dev/null

  step "Wait for the stack restore to finish"
  local SLOG="" i finished=""
  for i in $(seq 1 100); do
    SLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/stack:$PROJ/log" || true)"
    case "$SLOG" in
      *"restored — all"*) finished=1; break ;;
      *'"level":"ERR"'*|*"Stack restore CANCELED"*|*"What happened to each service"*)
        printf '%s\n' "$SLOG" | python3 -c "import sys,json;print(chr(10).join('    '+l['level']+' '+l['msg'] for l in json.load(sys.stdin).get('lines',[])[-30:]))" >&2
        fail "the stack restore did not succeed" ;;
    esac
    echo "  [$i] restoring"
    sleep 3
  done
  [ -n "$finished" ] || fail "the stack restore did not finish in time"
  printf '%s\n' "$SLOG" | python3 -c "import sys,json;print(chr(10).join('    '+l['msg'] for l in json.load(sys.stdin).get('lines',[]) if 'compose' in l['msg'] or '.env' in l['msg']))"

  step "Assert your own compose file and .env are the files Compose runs"
  cmp -s "$HOSTDIR/original-compose.yml" "$DIR/docker-compose.yml" || fail "docker-compose.yml is not the operator's own file"
  [ -f "$DIR/docker-compose.dockback.yml" ] || fail "the reconstruction is not beside it as docker-compose.dockback.yml"
  head -n 1 "$DIR/.env" | cmp -s - "$HOSTDIR/original.env" || fail "the .env does not start with the operator's own lines"
  [ "$(grep -c 'TOKEN=' "$DIR/.env")" = "1" ] || fail "the .env defines TOKEN more than once (the cloudflare duplicate)"
  [ "$(stat -c %a "$DIR/.env")" = "600" ] || fail "the .env is mode $(stat -c %a "$DIR/.env"), not 600"
  [ "$(tr -d '\n' < "$DIR/config/app.ini")" = "setting=$SUFFIX" ] || fail "the stack's config folder did not come back"

  step "Assert the project folder's other files came back, without its noise"
  [ -x "$DIR/scripts/nightly.sh" ] || fail "scripts/nightly.sh did not come back executable (the forgejo case)"
  grep -q "nightly $SUFFIX" "$DIR/scripts/nightly.sh" || fail "scripts/nightly.sh came back with the wrong content"
  [ -f "$DIR/README.md" ] || fail "README.md did not come back"
  [ ! -e "$DIR/node_modules" ] || fail "node_modules was captured, though it is rebuilt by a tool"
  [ ! -e "$DIR/debug.log" ] || fail "debug.log came back, though .dockbackignore leaves *.log out"

  step "Assert the reconstruction beside it is valid, joins what it shares, and keeps \$ literal"
  local RECON_JSON RECON_ERR
  RECON_ERR="$(mktemp)"
  RECON_JSON="$(docker compose -p "$PROJ" --project-directory "$DIR" -f "$DIR/docker-compose.dockback.yml" config --format json 2>"$RECON_ERR")" \
    || { cat "$RECON_ERR" >&2; rm -f "$RECON_ERR"; fail "the reconstruction is not a valid Compose file"; }
  if grep -q 'variable is not set' "$RECON_ERR"; then
    cat "$RECON_ERR" >&2; rm -f "$RECON_ERR"; fail "Compose interpolated a literal \$ in the reconstruction"
  fi
  rm -f "$RECON_ERR"
  python3 -c '
import json, sys
net, vol = sys.argv[1], sys.argv[2]
d = json.load(sys.stdin)
n = d.get("networks", {}).get(net, {})
assert n.get("external") is True and n.get("name") == net, "the shared network is re-declared: %s" % n
v = d.get("volumes", {}).get(vol, {})
assert v.get("external") is True and v.get("name") == vol, "the named volume is not declared: %s" % v
h = d["services"]["app"]["environment"]["HASH"]
assert h == "$$apr1$$e2e", "HASH reads %r" % h
print("  shared network external, named volume declared, $ kept literal")
' "$NET" "${PROJ}_data" <<<"$RECON_JSON" || fail "the reconstruction breaks a shared network, a volume or a \$ value"

  step "Assert \`docker compose up -d\` from the restored folder changes nothing"
  "${compose_in_dir[@]}" config -q || fail "the restored compose file is not valid"
  "${compose_in_dir[@]}" up -d >/dev/null 2>&1 || fail "docker compose up -d failed in the restored folder"
  [ "$(docker ps -q --no-trunc --filter "label=com.docker.compose.project=$PROJ")" = "$BEFORE" ] \
    || fail "docker compose up -d recreated the restored containers"
  case "$SLOG" in
    *"Checked with Docker Compose"*"docker-compose.yml is valid, and"*"will change nothing"*)
      echo "  the restore's own Compose check (step 15) agrees: nothing would change" ;;
    *) fail "the restore did not check the written compose file with Docker Compose (step 15)" ;;
  esac

  printf '\033[1;32mE2E PASSED (stack-folder): the deleted folder came back with your own compose file and .env, and Compose found nothing to change.\033[0m\n'
}

# ── case: files-only ─────────────────────────────────────────────────────────

case_files_only() {
  HOSTDIR="$(mktemp -d /tmp/dback-e2e-files-XXXXXX)"
  local PROJ="dback-e2e-files-$SUFFIX"
  local DIR="$HOSTDIR/$PROJ"
  EXTRA_PROJECTS+=("$PROJ")
  local compose_in_dir=(docker compose -p "$PROJ" --project-directory "$DIR" -f "$DIR/docker-compose.yml")

  step "Stand up a Compose project with a secret-file bind, a data folder and project files"
  mkdir -p "$DIR/data" "$DIR/scripts"
  printf 'records-%s\n' "$SUFFIX" > "$DIR/data/records.db"
  printf 'key-%s' "$SUFFIX" > "$DIR/app.key"
  chmod 600 "$DIR/app.key"
  printf '#!/bin/sh\necho maintenance\n' > "$DIR/scripts/maintenance.sh"
  chmod 755 "$DIR/scripts/maintenance.sh"
  cat > "$DIR/docker-compose.yml" <<EOF
services:
  app:
    image: alpine
    command: ["sleep", "600"]
    volumes:
      - ./data:/data
      - ./app.key:/run/secrets/app.key:ro
EOF
  printf 'APP_MODE=production\n' > "$DIR/.env"
  "${compose_in_dir[@]}" up -d --quiet-pull >/dev/null 2>&1 || fail "the test stack did not come up"

  step "Back it up"
  local CID BID STARTED
  CID="$(find_container "$PROJ-app-1")"
  BID="$(backup_and_verify "$CID" "$PROJ-app-1")"
  echo "  backup id: $BID"
  STARTED="$(docker inspect -f '{{.State.StartedAt}}' "$PROJ-app-1")"

  step "The disaster: delete the stack folder while the stack keeps running"
  rm -rf "$DIR"

  step "Restore the stack's files only"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/nodes/local/stacks/$PROJ/restore?files_only=true" >/dev/null

  step "Wait for the files-only restore to finish"
  local SLOG="" i finished=""
  for i in $(seq 1 60); do
    SLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/stack:$PROJ/log" || true)"
    case "$SLOG" in
      *"files restored — no service was stopped or changed"*) finished=1; break ;;
      *'"level":"ERR"'*)
        printf '%s\n' "$SLOG" | python3 -c "import sys,json;print(chr(10).join('    '+l['level']+' '+l['msg'] for l in json.load(sys.stdin).get('lines',[])[-30:]))" >&2
        fail "the files-only restore did not succeed" ;;
    esac
    echo "  [$i] restoring"
    sleep 3
  done
  [ -n "$finished" ] || fail "the files-only restore did not finish in time"

  step "Assert the files came back and nothing was touched"
  [ -f "$DIR/docker-compose.yml" ] || fail "the compose file did not come back"
  grep -q 'APP_MODE=production' "$DIR/.env" || fail "the .env did not come back"
  [ -x "$DIR/scripts/maintenance.sh" ] || fail "the project's script did not come back"
  [ -f "$DIR/app.key" ] || fail "the secret-file bind did not come back"
  [ "$(cat "$DIR/app.key")" = "key-$SUFFIX" ] || fail "the secret-file bind came back with the wrong content"
  [ "$(stat -c %a "$DIR/app.key")" = "600" ] || fail "the secret-file bind came back as mode $(stat -c %a "$DIR/app.key")"
  [ ! -e "$DIR/data/records.db" ] || fail "files-only restored data, which only a full restore may do"
  [ "$(docker inspect -f '{{.State.StartedAt}}' "$PROJ-app-1")" = "$STARTED" ] || fail "files-only restarted the container"
  case "$SLOG" in
    *"data folders are gone from the host"*) echo "  the missing data folder was named for a full restore" ;;
    *) fail "the missing data folder was not reported" ;;
  esac
  case "$SLOG" in
    *"Checked with Docker Compose"*"is valid, and"*"will change nothing"*) echo "  the restored compose file was checked with Docker Compose" ;;
    *) fail "the files-only restore did not check the compose file with Docker Compose" ;;
  esac

  step "Edit the compose file and .env, then restore the files only again"
  printf '# edited after the backup\n' >> "$DIR/docker-compose.yml"
  printf 'APP_MODE=staging\n' > "$DIR/.env"
  local compose_sum env_sum
  compose_sum="$(sha256sum < "$DIR/docker-compose.yml")"
  env_sum="$(sha256sum < "$DIR/.env")"
  files_only_rerun "$PROJ" "files restored — no service was stopped or changed"
  [ "$(sha256sum < "$DIR/docker-compose.yml")" = "$compose_sum" ] || fail "files-only replaced the edited compose file"
  [ "$(sha256sum < "$DIR/.env")" = "$env_sum" ] || fail "files-only replaced the edited .env"
  ! ls -A "$DIR" | grep -q 'pre-dockback-restore' || fail "files-only renamed an existing file aside: $(ls -A "$DIR" | tr '\n' ' ')"
  case "$SLOG" in
    *"kept exactly as they are"*) echo "  the edited files were kept, and the log says so" ;;
    *) fail "the log does not say the existing files were kept" ;;
  esac

  step "A stack folder that cannot be written fails the files-only restore"
  rm -rf "$DIR"
  : > "$DIR"
  files_only_rerun "$PROJ" "did not put back every file"
  rm -f "$DIR"
  echo "  the restore failed and named what it could not write"

  printf '\033[1;32mE2E PASSED (files-only): the files came back with the container never stopped, existing files were kept, a failed write failed the restore, and the missing data was named.\033[0m\n'
}

# files_only_rerun starts another files-only restore of a stack and waits until
# its log says want once more than it did before, leaving the log in SLOG. The
# stack's log is shared across runs, so earlier lines are counted out.
files_only_rerun() {
  local project="$1" want="$2" before i
  before="$(curl -fsS -b "$JAR" "$BASE/api/backups/stack:$project/log" | { grep -o "$want" || true; } | wc -l)"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/nodes/local/stacks/$project/restore?files_only=true" >/dev/null
  for i in $(seq 1 60); do
    SLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/stack:$project/log" || true)"
    [ "$(printf '%s' "$SLOG" | { grep -o "$want" || true; } | wc -l)" -gt "$before" ] && return 0
    echo "  [$i] restoring"
    sleep 3
  done
  fail "the files-only restore never logged: $want"
}

# ── case: stack-selection ────────────────────────────────────────────────────

case_stack_selection() {
  HOSTDIR="$(mktemp -d /tmp/dback-e2e-select-XXXXXX)"
  local PROJ="dback-e2e-select-$SUFFIX"
  local DIR="$HOSTDIR/$PROJ"
  local KEPT="$PROJ-kept-1" LEFT_OUT="$PROJ-left-out-1"
  local ORIGINAL="original-$SUFFIX" TAMPERED="tampered-$SUFFIX" DEST_NAME="e2e-offsite"
  EXTRA_PROJECTS+=("$PROJ")
  local compose_in_dir=(docker compose -p "$PROJ" --project-directory "$DIR" -f "$DIR/docker-compose.yml")
  marker() { docker exec "$1" cat /data/marker | tr -d '[:space:]'; }
  write_markers() {
    local c
    for c in "$KEPT" "$LEFT_OUT"; do docker exec "$c" sh -c "echo '$1' > /data/marker"; done
  }
  # wait_for_stack_restores <count> — until that many restores of the stack finished.
  wait_for_stack_restores() {
    local i
    for i in $(seq 1 100); do
      SLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/stack:$PROJ/log" || true)"
      case "$SLOG" in
        *'"level":"ERR"'*|*"Stack restore CANCELED"*|*"What happened to each service"*)
          printf '%s\n' "$SLOG" | python3 -c "import sys,json;print(chr(10).join('    '+l['level']+' '+l['msg'] for l in json.load(sys.stdin).get('lines',[])[-30:]))" >&2
          fail "the stack restore did not succeed" ;;
      esac
      [ "$(printf '%s' "$SLOG" | jget 'sum("restored — all" in l["msg"] for l in d.get("lines",[]))')" = "$1" ] && return 0
      echo "  [$i] restoring"
      sleep 3
    done
    fail "the stack restore did not finish in time"
  }

  step "Stand up a two-service Compose project, each service with its own volume"
  mkdir -p "$DIR"
  cat > "$DIR/docker-compose.yml" <<EOF
services:
  kept:
    image: alpine
    command: ["sleep", "600"]
    volumes:
      - kept-data:/data
  left-out:
    image: alpine
    command: ["sleep", "600"]
    volumes:
      - left-out-data:/data
volumes:
  kept-data:
  left-out-data:
EOF
  "${compose_in_dir[@]}" up -d --quiet-pull >/dev/null 2>&1 || fail "the test stack did not come up"
  write_markers "$ORIGINAL"

  step "Add a second copy for the restore to read from"
  local DEST_ID
  DEST_ID="$(curl -fsS "${auth[@]}" -X POST "$BASE/api/destinations" \
    -d "{\"name\":\"$DEST_NAME\",\"type\":\"local\",\"config\":{\"path\":\"/app/backups/$DEST_NAME\"}}" | jget 'd["id"]')"
  [ -n "$DEST_ID" ] || fail "could not add the destination"

  step "Back up both services to it"
  local KEPT_BID LEFT_OUT_BID
  KEPT_BID="$(backup_and_verify "$(find_container "$KEPT")" "$KEPT" "$DEST_ID")"
  LEFT_OUT_BID="$(backup_and_verify "$(find_container "$LEFT_OUT")" "$LEFT_OUT" "$DEST_ID")"
  echo "  backup ids: $KEPT_BID $LEFT_OUT_BID"

  step "Assert the plan offers that copy for both services"
  local OFFERED="" i
  for i in $(seq 1 30); do
    OFFERED="$(curl -fsS -b "$JAR" "$BASE/api/nodes/local/stacks/$PROJ/restore-plan" \
      | jget '[c["services"] for c in d.get("source_copies") or [] if c["id"]=="'"$DEST_ID"'"]')"
    [ "$OFFERED" = "[2]" ] && break
    [ "$i" = 30 ] && fail "the plan does not offer the copy for both services (got $OFFERED)"
    sleep 2
  done

  step "The damage: both services' data changes"
  write_markers "$TAMPERED"
  local LEFT_OUT_ID LEFT_OUT_STARTED
  LEFT_OUT_ID="$(docker inspect -f '{{.Id}}' "$LEFT_OUT")"
  LEFT_OUT_STARTED="$(docker inspect -f '{{.State.StartedAt}}' "$LEFT_OUT")"

  # No safety snapshot: it would be the service's newest backup for the second
  # restore below, which has to read this same backup again.
  local RESTORE_URL="$BASE/api/nodes/local/stacks/$PROJ/restore?service=kept&source=$DEST_ID&snapshot=false"

  step "Restore one service, from the chosen copy"
  curl -fsS "${auth[@]}" -X POST "$RESTORE_URL" >/dev/null
  local SLOG=""
  wait_for_stack_restores 1

  step "Assert only the named service changed"
  [ "$(marker "$KEPT")" = "$ORIGINAL" ] || fail "the named service did not get its data back (got '$(marker "$KEPT")')"
  [ "$(marker "$LEFT_OUT")" = "$TAMPERED" ] || fail "the service left out was overwritten"
  [ "$(docker inspect -f '{{.Id}}' "$LEFT_OUT")" = "$LEFT_OUT_ID" ] || fail "the service left out was recreated"
  [ "$(docker inspect -f '{{.State.StartedAt}}' "$LEFT_OUT")" = "$LEFT_OUT_STARTED" ] || fail "the service left out was restarted"
  case "$SLOG" in
    *"Restoring 1 of the stack's services"*) echo "  the run says it covered one service" ;;
    *) fail "the run log does not say the restore was narrowed" ;;
  esac

  step "Assert the named service was read from the chosen copy alone"
  local RESTORE_LOG
  RESTORE_LOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$KEPT_BID/log" || true)"
  case "$RESTORE_LOG" in
    *"Restoring from local"*) fail "the chosen copy was not the only one read: the local copy was too" ;;
    *"Restoring from $DEST_NAME (dest)"*) echo "  read from $DEST_NAME" ;;
    *) fail "the log does not name the copy the service was read from" ;;
  esac
  case "$SLOG" in
    *"no readable copy on the chosen source"*) fail "the console warned of a fallback that did not happen" ;;
  esac

  step "Corrupt the chosen copy: the restore must still work, and say it fell back"
  local KEPT_KEY
  KEPT_KEY="$(curl -fsS -b "$JAR" "$BASE/api/backups/$KEPT_BID" | jget 'd["storage_key"]')"
  docker run --rm -v "${PROJECT}_e2e-backups":/b alpine sh -c "printf corrupted >> '/b/$DEST_NAME/$KEPT_KEY'" \
    || fail "could not corrupt the chosen copy"
  write_markers "$TAMPERED"
  curl -fsS "${auth[@]}" -X POST "$RESTORE_URL" >/dev/null
  wait_for_stack_restores 2
  [ "$(marker "$KEPT")" = "$ORIGINAL" ] || fail "the restore did not fall back to a good copy (got '$(marker "$KEPT")')"
  case "$SLOG" in
    *"$KEPT: no readable copy on the chosen source — restoring from local (local) instead"*) echo "  the console says it fell back" ;;
    *) fail "the restore read another copy than the chosen one without saying so" ;;
  esac

  step "A name the stack does not have is refused, and nothing is touched"
  write_markers "$TAMPERED"
  local KEPT_STARTED refused=""
  KEPT_STARTED="$(docker inspect -f '{{.State.StartedAt}}' "$KEPT")"
  # The finished run's lock is released just after its last log line.
  for i in $(seq 1 10); do
    curl -fsS "${auth[@]}" -X POST "$BASE/api/nodes/local/stacks/$PROJ/restore?service=ghost" >/dev/null 2>&1 && break
    [ "$i" = 10 ] && fail "the stack stayed locked after the restore finished"
    sleep 1
  done
  for i in $(seq 1 20); do
    case "$(curl -fsS -b "$JAR" "$BASE/api/backups/stack:$PROJ/log" || true)" in
      *"no service named ghost"*) refused=1; break ;;
    esac
    sleep 1
  done
  [ -n "$refused" ] || fail "an unknown service was not refused"
  [ "$(marker "$KEPT")" = "$TAMPERED" ] || fail "a refused restore changed the data"
  [ "$(marker "$LEFT_OUT")" = "$TAMPERED" ] || fail "a refused restore changed the data"
  [ "$(docker inspect -f '{{.State.StartedAt}}' "$KEPT")" = "$KEPT_STARTED" ] || fail "a refused restore restarted a service"
  [ "$(docker inspect -f '{{.State.StartedAt}}' "$LEFT_OUT")" = "$LEFT_OUT_STARTED" ] || fail "a refused restore restarted a service"

  printf '\033[1;32mE2E PASSED (stack-selection): the named service came back from the chosen copy, and the other was never touched.\033[0m\n'
}

# ── case: clone-ownership ────────────────────────────────────────────────────

case_clone_ownership() {
  local SENTINEL_NAME="$TARGET-sentinel" STANDBY_NEIGHBOUR="$TARGET-standby"
  local COPY="$TARGET-copy" SECOND_NODE="e2e-second-node"
  EXTRA_CONTAINERS+=("$SENTINEL_NAME" "$STANDBY_NEIGHBOUR" "$COPY")
  # state <container> — everything a copy or a rehearsal must leave alone.
  state() { docker inspect -f '{{.Id}} {{.State.Running}} {{.State.StartedAt}}' "$1"; docker exec "$1" cat /marker; }

  step "Create a target to back up, and two unrelated containers"
  docker run -d --name "$TARGET" -e APP_MARKER="$SENTINEL" alpine sleep 600 >/dev/null
  local c
  for c in "$SENTINEL_NAME" "$STANDBY_NEIGHBOUR"; do
    docker run -d --name "$c" alpine sh -c "echo '$SENTINEL' > /marker; exec sleep 600" >/dev/null
  done
  local CID BID SENTINEL_BEFORE NEIGHBOUR_BEFORE i
  CID="$(find_container "$TARGET")"
  BID="$(backup_and_verify "$CID" "$TARGET")"
  for i in $(seq 1 10); do
    SENTINEL_BEFORE="$(state "$SENTINEL_NAME" 2>/dev/null || true)"
    case "$SENTINEL_BEFORE" in *"$SENTINEL"*) break ;; esac
    sleep 1
  done
  NEIGHBOUR_BEFORE="$(state "$STANDBY_NEIGHBOUR")"

  # copy_status <name> <recreate> — the HTTP status of a restore-as-a-copy request.
  copy_status() {
    curl -s -o /dev/null -w '%{http_code}' "${auth[@]}" -X POST "$BASE/api/backups/$BID/restore" \
      -d "{\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"confirm\":true,\"as_name\":\"$1\",\"isolated\":true,\"recreate\":$2}"
  }

  step "A copy under a name in use is refused, with or without recreate"
  [ "$(copy_status "$SENTINEL_NAME" false)" = "409" ] || fail "a copy under a name in use was not refused"
  [ "$(copy_status "$SENTINEL_NAME" true)" = "409" ] || fail "a recreating copy under a name in use was not refused"
  [ "$(state "$SENTINEL_NAME")" = "$SENTINEL_BEFORE" ] || fail "the container holding the name changed"

  step "A copy that is not isolated is refused"
  [ "$(curl -s -o /dev/null -w '%{http_code}' "${auth[@]}" -X POST "$BASE/api/backups/$BID/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"confirm\":true,\"as_name\":\"$COPY\"}")" = "400" ] \
    || fail "a copy that is not isolated was not refused"

  step "A free name still works"
  [ "$(copy_status "$COPY" false)" = "202" ] || fail "a copy under a free name was not accepted"
  for i in $(seq 1 40); do
    [ "$(docker inspect -f '{{.State.Running}}' "$COPY" 2>/dev/null || true)" = "true" ] && break
    [ "$i" = 40 ] && fail "the copy never came up"
    sleep 3
  done
  echo "  copy is up beside the original"

  step "Rehearse standby onto a second node, beside a container called <target>-standby"
  local SECOND_ID
  SECOND_ID="$(curl -fsS "${auth[@]}" -X POST "$BASE/api/nodes" \
    -d "{\"name\":\"$SECOND_NODE\",\"transport\":\"tcp-proxy\",\"address\":\"tcp://socket-proxy:2375\"}" | jget 'd["id"]')"
  [ -n "$SECOND_ID" ] || fail "could not add the second node"
  curl -fsS "${auth[@]}" -X PUT "$BASE/api/nodes/local/containers/$CID/standby" \
    -d "{\"standby_node\":\"$SECOND_ID\",\"interval_days\":30}" >/dev/null
  curl -fsS "${auth[@]}" -X POST "$BASE/api/nodes/local/containers/$CID/standby/run" >/dev/null
  local RESULT=""
  for i in $(seq 1 60); do
    RESULT="$(curl -fsS -b "$JAR" "$BASE/api/nodes/local/containers/$CID/standby" || true)"
    # A rehearsal that has not run yet also reads last_ok false; last_run says it ran.
    case "$RESULT" in
      *'"last_run":0'*|*'"last_run":null'*) ;;
      *'"last_ok":true'*) break ;;
      *'"last_ok":false'*) fail "the standby rehearsal failed: $RESULT" ;;
    esac
    [ "$i" = 60 ] && fail "the standby rehearsal did not finish: $RESULT"
    sleep 3
  done
  echo "  rehearsal passed"

  step "Assert the neighbour is unchanged and the rehearsal left nothing"
  [ "$(state "$STANDBY_NEIGHBOUR")" = "$NEIGHBOUR_BEFORE" ] || fail "the rehearsal touched a container it did not create"
  [ -z "$(docker ps -a --format '{{.Names}}' | grep -F "$STANDBY_NEIGHBOUR-" || true)" ] || fail "the rehearsal left its clone behind"
  [ "$(state "$SENTINEL_NAME")" = "$SENTINEL_BEFORE" ] || fail "the container holding the name changed"

  printf '\033[1;32mE2E PASSED (clone-ownership): copies and rehearsals created their own containers and touched nothing else.\033[0m\n'
}

# ── case: clone-sandbox ──────────────────────────────────────────────────────

case_clone_sandbox() {
  local COPY="$TARGET-copy" LISTENER="$TARGET-listener" PG="$TARGET-pg" PG_COPY="$TARGET-pg-copy"
  EXTRA_CONTAINERS+=("$COPY" "$LISTENER" "$PG" "$PG_COPY")

  # copy_of <backup-id> <container-id> <name> — request a copy, wait until it runs.
  copy_of() {
    curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$1/restore" \
      -d "{\"node_id\":\"local\",\"target_id\":\"$2\",\"volumes\":true,\"database\":true,\"confirm\":true,\"as_name\":\"$3\",\"isolated\":true}" >/dev/null
    local i
    for i in $(seq 1 40); do
      [ "$(docker inspect -f '{{.State.Running}}' "$3" 2>/dev/null || true)" = "true" ] && return 0
      sleep 3
    done
    fail "the copy $3 never came up"
  }

  step "Back up a source holding a device, an extra capability and the host's PID namespace"
  docker run -d --name "$TARGET" --cap-add NET_ADMIN --pid host \
    --device /dev/null:/dev/sentinel-device alpine sleep 600 >/dev/null
  local CID BID
  CID="$(find_container "$TARGET")"
  BID="$(backup_and_verify "$CID" "$TARGET")"

  step "Start a listener the source's bridge can reach"
  docker run -d --name "$LISTENER" alpine sh -c 'while true; do echo reached | nc -l -p 8080; done' >/dev/null
  local LISTENER_IP
  LISTENER_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$LISTENER")"

  step "Bring up a copy"
  copy_of "$BID" "$CID" "$COPY"

  step "Assert the copy has none of it"
  [ "$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$COPY")" = "none" ] || fail "the copy has a network"
  [ "$(docker inspect -f '{{len .HostConfig.CapAdd}} {{len .HostConfig.Devices}} {{.HostConfig.PidMode}}' "$COPY")" = "0 0 " ] \
    || fail "the copy kept a host power: $(docker inspect -f '{{.HostConfig.CapAdd}} {{.HostConfig.Devices}} {{.HostConfig.PidMode}}' "$COPY")"
  docker exec "$COPY" test ! -e /dev/sentinel-device || fail "the copy has the source's device"
  docker exec "$COPY" nc -w 2 "$LISTENER_IP" 8080 >/dev/null 2>&1 && fail "the copy reached a listener on the bridge"
  docker exec "$COPY" nc -w 2 1.1.1.1 53 </dev/null >/dev/null 2>&1 && fail "the copy reached the internet"
  [ "$(docker exec "$TARGET" nc -w 2 "$LISTENER_IP" 8080 2>/dev/null | tr -d '[:space:]')" = "reached" ] \
    || fail "the source cannot reach the listener either, so this proved nothing"
  echo "  no network, no device, no capability, no host namespace"

  step "Assert the log names what the copy lost"
  case "$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" || true)" in
    *"capabilities NET_ADMIN"*"device /dev/null"*"shared PID namespace"*) echo "  named" ;;
    *) fail "the log does not name what the sandbox removed" ;;
  esac

  step "The documented way out: an operator can still put a copy on a network"
  docker network disconnect none "$COPY" || fail "could not take the copy off 'none'"
  docker network connect bridge "$COPY" || fail "could not attach the copy to a network"
  [ "$(docker exec "$COPY" nc -w 2 "$LISTENER_IP" 8080 2>/dev/null | tr -d '[:space:]')" = "reached" ] || fail "the attached copy cannot reach the listener"

  step "A database copy still gets its data back with no network"
  docker run -d --name "$PG" -e POSTGRES_PASSWORD=e2e postgres:16-alpine >/dev/null
  # Retried, not gated on pg_isready: the image's init server answers it too, and
  # then shuts down under the first connection.
  local i
  for i in $(seq 1 60); do
    docker exec "$PG" psql -U postgres -q -c "create table marker(v text); insert into marker values ('$SENTINEL');" >/dev/null 2>&1 && break
    [ "$i" = 60 ] && fail "seeding failed"
    sleep 1
  done
  local PG_CID PG_BID
  PG_CID="$(find_container "$PG")"
  PG_BID="$(backup_and_verify "$PG_CID" "$PG")"
  copy_of "$PG_BID" "$PG_CID" "$PG_COPY"
  for i in $(seq 1 40); do
    [ "$(docker exec "$PG_COPY" psql -U postgres -Atqc 'select v from marker' 2>/dev/null || true)" = "$SENTINEL" ] && break
    [ "$i" = 40 ] && fail "the database copy never got its data"
    sleep 3
  done
  [ "$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$PG_COPY")" = "none" ] || fail "the database copy has a network"
  echo "  the database copy has its rows"

  printf '\033[1;32mE2E PASSED (clone-sandbox): copies came up with no network and no host powers, and a database copy still got its data.\033[0m\n'
}

# ── case: revert-rollback ────────────────────────────────────────────────────

case_revert_rollback() {
  local OLD_IMAGE="alpine:3.20" NEW_IMAGE="alpine:latest" HEALTH_TIMEOUT_SECONDS=30
  local OLD_MARKER="old-$SUFFIX" NEW_MARKER="new-$SUFFIX"
  docker volume create "$VOL" >/dev/null
  marker() { docker run --rm -v "$VOL":/d:ro alpine cat /d/marker | tr -d '[:space:]'; }
  image_of() { docker inspect -f '{{.Config.Image}}' "$TARGET"; }

  step "Back up the container on the older image, with a health check that always fails"
  docker run -d --name "$TARGET" -v "$VOL":/data --health-cmd false --health-interval 2s --health-retries 1 \
    "$OLD_IMAGE" sh -c "echo '$OLD_MARKER' > /data/marker; exec sleep 600" >/dev/null
  local BID
  BID="$(backup_and_verify "$(find_container "$TARGET")" "$TARGET")"

  step "Upgrade it: a newer image, no health check, newer data"
  docker rm -f "$TARGET" >/dev/null
  docker run -d --name "$TARGET" -v "$VOL":/data "$NEW_IMAGE" sh -c "echo '$NEW_MARKER' > /data/marker; exec sleep 600" >/dev/null
  local NEW_CID="" i
  for i in $(seq 1 30); do
    NEW_CID="$(curl -fsS -b "$JAR" "$BASE/api/nodes/local/containers?q=$TARGET&page_size=5" \
      | jget '[c["id"] for c in d["containers"] if c["image"]=="'"$NEW_IMAGE"'"][0] if [c for c in d["containers"] if c["image"]=="'"$NEW_IMAGE"'"] else ""' 2>/dev/null || true)"
    [ -n "$NEW_CID" ] && break
    sleep 2
  done
  [ -n "$NEW_CID" ] || fail "the upgraded container was not discovered"

  step "Shorten the health gate, then revert"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/settings" -d "{\"restore.health_timeout_seconds\":\"$HEALTH_TIMEOUT_SECONDS\"}" >/dev/null \
    || fail "could not shorten the health gate"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$NEW_CID\",\"volumes\":true,\"database\":true,\"snapshot\":true,\"recreate\":true,\"confirm\":true}" >/dev/null

  step "Wait for the rollback"
  local RLOG="" rolled=""
  for i in $(seq 1 80); do
    RLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" || true)"
    case "$RLOG" in
      *"Rolled back to the pre-restore snapshot"*) rolled=1; break ;;
      *"Rollback to the safety snapshot FAILED"*) fail "the rollback failed" ;;
    esac
    sleep 3
  done
  [ -n "$rolled" ] || fail "the restore never rolled back"

  step "Assert the image, the configuration and the data are what they were"
  [ "$(image_of)" = "$NEW_IMAGE" ] || fail "rolled back into the wrong image: $(image_of), with $NEW_IMAGE's data"
  [ "$(docker inspect -f '{{if .Config.Healthcheck}}{{.Config.Healthcheck.Test}}{{end}}' "$TARGET")" = "" ] || fail "the failed revert's health check survived the rollback"
  [ "$(marker)" = "$NEW_MARKER" ] || fail "the data is not what it was (got '$(marker)')"
  [ "$(docker inspect -f '{{.State.Running}}' "$TARGET")" = "true" ] || fail "the container is not running after the rollback"

  printf '\033[1;32mE2E PASSED (revert-rollback): the rollback brought back the image, configuration and data.\033[0m\n'
}

# ── case: shutdown-holds ─────────────────────────────────────────────────────

case_shutdown_holds() {
  local OPERATOR_PAUSED="$TARGET-operator" APP="$PROJECT-backup-app-1" LARGE_FILE_MB=400 i
  EXTRA_CONTAINERS+=("$OPERATOR_PAUSED")
  is_paused() { [ "$(docker inspect -f '{{.State.Paused}}' "$1")" = "true" ]; }

  # A file rewritten for as long as the container runs, so the copy made while it
  # is held takes long enough to stop DockBack during it.
  step "Create a container that keeps rewriting a large file, and one somebody else paused"
  docker volume create "$VOL" >/dev/null
  docker run -d --name "$TARGET" -v "$VOL":/data alpine sh -c \
    "while true; do dd if=/dev/zero of=/data/large bs=1M count=$LARGE_FILE_MB 2>/dev/null; done" >/dev/null
  docker run -d --name "$OPERATOR_PAUSED" alpine sleep 600 >/dev/null
  docker pause "$OPERATOR_PAUSED" >/dev/null
  local CID
  CID="$(find_container "$TARGET")"

  # stop_during_hold <signal> — start a backup, wait for its pause, send the signal.
  stop_during_hold() {
    curl -fsS "${auth[@]}" -X POST "$BASE/api/backups" \
      -d "{\"node_id\":\"local\",\"container_id\":\"$CID\",\"compression\":\"balanced\",\"destinations\":[]}" >/dev/null
    for i in $(seq 1 1500); do
      is_paused "$TARGET" && break
      [ "$i" = 1500 ] && fail "the backup never paused the container"
      sleep 0.1
    done
    docker kill -s "$1" "$APP" >/dev/null
    for i in $(seq 1 90); do
      [ "$(docker inspect -f '{{.State.Running}}' "$APP")" = "false" ] && return 0
      sleep 1
    done
    fail "DockBack did not exit after $1"
  }

  step "SIGTERM during the hold: the container is released before DockBack exits"
  stop_during_hold TERM
  is_paused "$TARGET" && fail "DockBack exited on SIGTERM and left the container paused"
  [ "$(docker inspect -f '{{.State.ExitCode}}' "$APP")" = "0" ] || fail "a clean drain should exit 0, got $(docker inspect -f '{{.State.ExitCode}}' "$APP")"
  echo "  released, and DockBack exited cleanly"

  step "Bring DockBack back; the interrupted backup runs again and pauses again"
  docker start "$APP" >/dev/null
  log_in
  for i in $(seq 1 1500); do
    is_paused "$TARGET" && break
    sleep 0.1
  done
  for i in $(seq 1 300); do
    is_paused "$TARGET" || break
    sleep 1
  done
  is_paused "$TARGET" && fail "the re-run backup never let the container go"
  # Released is not finished: a backup requested before it finishes would join it.
  for i in $(seq 1 120); do
    [ "$(curl -fsS -b "$JAR" "$BASE/api/backups?node_id=local&page_size=5" | jget 'sum(b["status"]=="running" for b in d["items"])')" = "0" ] && break
    [ "$i" = 120 ] && fail "the re-run backup never finished"
    sleep 2
  done
  sleep 3

  step "SIGKILL during the hold: nothing can release it until DockBack is back"
  stop_during_hold KILL
  is_paused "$TARGET" || fail "the container was not left paused, so this proves nothing"
  docker start "$APP" >/dev/null
  log_in
  for i in $(seq 1 30); do
    is_paused "$TARGET" || break
    sleep 1
  done
  is_paused "$TARGET" && fail "the restarted DockBack did not release the hold it owned"
  echo "  released by the restarted instance"

  step "Assert the container somebody else paused is still paused"
  is_paused "$OPERATOR_PAUSED" || fail "DockBack unpaused a container it did not pause"

  printf '\033[1;32mE2E PASSED (shutdown-holds): holds were released on SIGTERM and after SIGKILL, and only those.\033[0m\n'
}

# ── case: hook-cleanup ───────────────────────────────────────────────────────

case_hook_cleanup() {
  local APP="$PROJECT-backup-app-1" HOOK_LOG="/tmp/hooks.log" i
  docker run -d --name "$TARGET" alpine sleep 900 >/dev/null
  local CID
  CID="$(find_container "$TARGET")"
  # set_hooks <pre-line> — the post-hook is always the same line.
  set_hooks() {
    curl -fsS "${auth[@]}" -X PUT "$BASE/api/nodes/local/containers/$CID/hooks" \
      -d "{\"pre\":[\"$1\"],\"post\":[\"echo off >> $HOOK_LOG\"],$CONFIRM_PASSWORD}" >/dev/null
    docker exec "$TARGET" sh -c ": > $HOOK_LOG"
  }
  hook_log() { docker exec "$TARGET" cat "$HOOK_LOG" | tr '\n' ' '; }
  start_backup() {
    curl -fsS "${auth[@]}" -X POST "$BASE/api/backups" \
      -d "{\"node_id\":\"local\",\"container_id\":\"$CID\",\"compression\":\"balanced\",\"destinations\":[]}" | jget 'd.get("backup_id") or d.get("id") or ""'
  }
  wait_for_hook_log() {
    for i in $(seq 1 60); do
      [ "$(hook_log)" = "$1" ] && return 0
      sleep 1
    done
    fail "$2 (hook log: '$(hook_log)')"
  }

  step "A pre-hook that changes state and then fails still gets its post-hook"
  set_hooks "echo on >> $HOOK_LOG; exit 1"
  start_backup >/dev/null
  wait_for_hook_log "on off " "the post-hook did not run after a failed pre-hook"
  echo "  undone"

  step "A good run ends its window before it compresses"
  set_hooks "echo on >> $HOOK_LOG"
  local BID
  BID="$(backup_and_verify "$CID" "$TARGET")"
  [ "$(hook_log)" = "on off " ] || fail "the hooks did not pair (hook log: '$(hook_log)')"
  curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" | python3 -c '
import json, sys
msgs = [l["msg"] for l in json.load(sys.stdin)["lines"]]
post = next(i for i, m in enumerate(msgs) if m.startswith("post-hook"))
tail = next(i for i, m in enumerate(msgs) if m.startswith("Compressing"))
assert post < tail, "the post-hook ran at line %d, after compression began at line %d" % (post, tail)
print("  post-hook ran before compression")
' || fail "the window lasted into the upload"

  step "DockBack killed inside a window runs the post-hook when it returns"
  set_hooks "echo on >> $HOOK_LOG; sleep 300"
  start_backup >/dev/null
  wait_for_hook_log "on " "the pre-hook never started"
  docker kill -s KILL "$APP" >/dev/null
  docker start "$APP" >/dev/null
  # The interrupted backup then runs again and opens a new window, so only the
  # start of the log is this one's.
  for i in $(seq 1 60); do
    case "$(hook_log)" in "on off "*) break ;; esac
    [ "$i" = 60 ] && fail "the restarted DockBack did not run the post-hook (hook log: '$(hook_log)')"
    sleep 1
  done

  printf '\033[1;32mE2E PASSED (hook-cleanup): every pre-hook got its post-hook once, before the upload and across a crash.\033[0m\n'
}

# ── case: hold-required ──────────────────────────────────────────────────────

case_hold_required() {
  local PROJ="dback-e2e-hold-$SUFFIX" i
  EXTRA_PROJECTS+=("$PROJ")
  docker volume create "$VOL" >/dev/null
  docker run -d --name "$TARGET" -v "$VOL":/data alpine sh -c "echo x > /data/f; exec sleep 600" >/dev/null
  local CID STARTED
  CID="$(find_container "$TARGET")"
  STARTED="$(docker inspect -f '{{.State.StartedAt}}' "$TARGET")"

  # backup_with <pause-mode> — run a backup; print "<status>|<achieved>|<error>".
  backup_with() {
    local id="" status=""
    id="$(curl -fsS "${auth[@]}" -X POST "$BASE/api/backups" \
      -d "{\"node_id\":\"local\",\"container_id\":\"$CID\",\"compression\":\"balanced\",\"destinations\":[],\"pause_mode\":\"$1\"}" | jget 'd["backup_id"]')"
    for i in $(seq 1 60); do
      status="$(curl -fsS -b "$JAR" "$BASE/api/backups/$id" | jget 'd["status"]+"/"+d.get("verified","")')"
      case "$status" in failed/*|success/verified|success/failed) break ;; esac
      sleep 2
    done
    curl -fsS -b "$JAR" "$BASE/api/backups/$id" | python3 -c '
import json, sys
d = json.load(sys.stdin)
man = json.loads(d.get("manifest_json") or "{}")
print("|".join([d["status"], man.get("quiesce_achieved", ""), d.get("error", "")]))'
  }

  step "A backup that asked for a stop, which the proxy refuses, fails"
  local RESULT
  RESULT="$(backup_with stop)"
  echo "  $RESULT" | cut -c1-160
  case "$RESULT" in failed\|*) ;; *) fail "a backup succeeded without the stop it asked for: $RESULT" ;; esac
  case "$RESULT" in *"could not stop"*) ;; *) fail "the failure does not say what happened: $RESULT" ;; esac
  [ "$(docker inspect -f '{{.State.StartedAt}} {{.State.Running}}' "$TARGET")" = "$STARTED true" ] || fail "the container was disturbed"

  step "Pause and live modes still work, and say what they were"
  RESULT="$(backup_with pause)"
  case "$RESULT" in success\|pause\|*) echo "  pause: achieved pause" ;; *) fail "pause mode: $RESULT" ;; esac
  RESULT="$(backup_with none)"
  case "$RESULT" in success\|live\|*) echo "  none: recorded as live" ;; *) fail "live mode: $RESULT" ;; esac

  step "A stack whose stop is refused is not reported as one point in time"
  HOSTDIR="$(mktemp -d /tmp/dback-e2e-hold-XXXXXX)"
  cat > "$HOSTDIR/docker-compose.yml" <<EOF
services:
  one:
    image: alpine
    command: ["sleep", "600"]
    volumes: [one-data:/data]
  two:
    image: alpine
    command: ["sleep", "600"]
    volumes: [two-data:/data]
volumes:
  one-data:
  two-data:
EOF
  docker compose -p "$PROJ" --project-directory "$HOSTDIR" -f "$HOSTDIR/docker-compose.yml" up -d --quiet-pull >/dev/null 2>&1 || fail "the test stack did not come up"
  find_container "$PROJ-one-1" >/dev/null
  find_container "$PROJ-two-1" >/dev/null
  curl -fsS "${auth[@]}" -X POST "$BASE/api/nodes/local/stacks/$PROJ/backup" -d '{"consistent":true,"pause_mode":"stop"}' >/dev/null
  local SLOG="" finished=""
  for i in $(seq 1 60); do
    SLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/stack:$PROJ/log" || true)"
    case "$SLOG" in
      *"App-consistent snapshot complete"*) fail "the stack claimed one point in time without its holds" ;;
      *"App-consistent snapshot failed"*) finished=1; break ;;
    esac
    sleep 2
  done
  [ -n "$finished" ] || fail "the stack backup never finished"
  [ "$(curl -fsS -b "$JAR" "$BASE/api/nodes/local/stacks/$PROJ/groups" | jget 'sum(1 for g in (d or []) if g["complete"])')" = "0" ] \
    || fail "the group is offered as a complete snapshot"
  for s in one two; do
    [ "$(docker inspect -f '{{.State.Running}}' "$PROJ-$s-1")" = "true" ] || fail "service $s was left down"
  done
  echo "  failed, not offered as a snapshot, and both services still run"

  printf '\033[1;32mE2E PASSED (hold-required): a refused hold failed the backup instead of being called consistent.\033[0m\n'
}

# ── case: incremental-fidelity ───────────────────────────────────────────────

case_incremental_fidelity() {
  local i MARK_MTIME="202001010000.00"
  docker volume create "$VOL" >/dev/null
  docker run -d --name "$TARGET" -v "$VOL":/data alpine sleep 900 >/dev/null
  # in_volume <script> — run a script against the volume, as root, from outside.
  in_volume() { docker exec "$TARGET" sh -c "cd /data && $1"; }
  # tree — everything a restore has to bring back, one sorted line per entry.
  tree() {
    in_volume 'find . -mindepth 1 -exec stat -c "%n|%F|%a|%u|%g|%s" {} + | sort; for f in chmodded rewritten link-target/child "newline
name"; do [ -e "$f" ] && echo "$f=$(cat "$f")"; done; [ -L link ] && readlink link; true'
  }

  in_volume 'echo a > chmodded; chmod 644 chmodded; echo a > chowned; echo one > rewritten; touch -t '"$MARK_MTIME"' rewritten
    ln -s old-target link; echo f > file-to-dir; mkdir dir-to-file; echo x > dir-to-file/inner; echo gone > deleted; mkdir emptied-dir
    mkdir dir-to-link link-target; echo old > dir-to-link/child; echo keep > link-target/child'
  local CID BID_FULL BID_DELTA
  CID="$(find_container "$TARGET")"

  step "Turn incremental on, and take the full baseline"
  curl -fsS "${auth[@]}" -X PUT "$BASE/api/nodes/local/containers/$CID/backup-options" \
    -d '{"incremental":true,"incremental_full_every":5,"compression":"balanced"}' >/dev/null \
    || fail "could not turn incremental on"
  BID_FULL="$(backup_and_verify "$CID" "$TARGET")"
  sleep 2

  step "Change everything a size and mtime cannot see"
  in_volume 'chmod 600 chmodded; chown 1234:1234 chowned; echo two > rewritten; touch -t '"$MARK_MTIME"' rewritten
    rm link; ln -s new-target link; rm file-to-dir; mkdir file-to-dir; echo y > file-to-dir/inner
    rm -r dir-to-file; echo z > dir-to-file; rm deleted; mkdir new-empty-dir; rmdir emptied-dir
    rm -r dir-to-link; ln -s link-target dir-to-link'
  local WANT
  WANT="$(tree)"

  step "Take the delta"
  # backup_and_verify reads the newest row of this target, which is this run's.
  BID_DELTA="$(backup_and_verify "$CID" "$TARGET")"
  [ "$BID_DELTA" != "$BID_FULL" ] || fail "no new backup was taken"
  curl -fsS -b "$JAR" "$BASE/api/backups/$BID_DELTA" | jget 'json.loads(d["manifest_json"]).get("incremental", False)' | grep -q True \
    || fail "the second backup was not a delta, so this proves nothing"

  step "Damage the volume, restore the delta, and compare"
  in_volume 'find . -mindepth 1 -delete'
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID_DELTA/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"database\":false,\"confirm\":true}" >/dev/null
  for i in $(seq 1 60); do
    case "$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID_DELTA/log" || true)" in
      *"Restored container is healthy"*) break ;;
      *"Restore failed"*|*"Restore refused"*) fail "the restore failed" ;;
    esac
    [ "$i" = 60 ] && fail "the restore never finished"
    sleep 3
  done
  local GOT
  GOT="$(tree)"
  if [ "$GOT" != "$WANT" ]; then
    printf '  wanted:\n%s\n  got:\n%s\n' "$WANT" "$GOT" >&2
    fail "the restored tree is not what the delta captured"
  fi
  echo "  mode, owner, same-size write, link, empty directory, both type changes and the delete all came back"

  step "Rebuild the same tree from the archives alone, with the offline tool"
  local OFFLINE ARCHIVE
  HOSTDIR="$(mktemp -d /tmp/dback-e2e-offline-XXXXXX)"
  OFFLINE="$HOSTDIR"
  docker cp "$("${COMPOSE[@]}" ps -q backup-app):/app/backups" "$OFFLINE/backups" >/dev/null 2>&1 || fail "could not copy the archives out"
  ARCHIVE="$OFFLINE/backups/$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID_DELTA" | jget 'd["storage_key"]')"
  [ -f "$ARCHIVE" ] || fail "the delta's archive was not found at $ARCHIVE"
  python3 scripts/dockback-recover.py --key "$DOCKBACK_ENCRYPTION_KEY" --in "$ARCHIVE" \
    --apply-chain "$(dirname "$ARCHIVE")" --out "$OFFLINE/chain" >/dev/null || fail "--apply-chain failed"
  mkdir "$OFFLINE/chain/RESTORE_DIR"
  (cd "$OFFLINE/chain" && grep -E '^     (\(cd|tar) ' APPLY_ORDER.txt | sh) || fail "APPLY_ORDER.txt did not run"
  # Types, names and modes: only a root shell would bring the owners back.
  local OFFLINE_TREE
  OFFLINE_TREE="$(cd "$OFFLINE/chain/RESTORE_DIR/data" && find . -mindepth 1 -exec stat -c '%n|%F|%a' {} + | LC_ALL=C sort)"
  if [ "$OFFLINE_TREE" != "$(printf '%s\n' "$WANT" | grep '^\./' | cut -d'|' -f1-3 | LC_ALL=C sort)" ]; then
    printf '  got:\n%s\n' "$OFFLINE_TREE" >&2
    fail "the offline tree is not what the delta captured"
  fi
  [ "$(cat "$OFFLINE/chain/RESTORE_DIR/data/rewritten")" = two ] || fail "the offline tree has the old bytes"
  echo "  the offline tool rebuilt the same names, types and modes, and the same-size write"

  step "A name with a newline in it forces a true full, and comes back"
  in_volume 'printf nl > "$(printf "newline\nname")"'
  sleep 2
  local BID_NL
  BID_NL="$(backup_and_verify "$CID" "$TARGET")"
  curl -fsS -b "$JAR" "$BASE/api/backups/$BID_NL" | jget 'json.loads(d["manifest_json"]).get("incremental", False)' | grep -q False \
    || fail "a name a delta cannot carry must force a full"
  WANT="$(tree)"
  in_volume 'find . -mindepth 1 -delete'
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID_NL/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"database\":false,\"confirm\":true}" >/dev/null
  for i in $(seq 1 60); do
    case "$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID_NL/log" || true)" in
      *"Restored container is healthy"*) break ;;
    esac
    [ "$i" = 60 ] && fail "the restore never finished"
    sleep 3
  done
  [ "$(tree)" = "$WANT" ] || fail "the newline-named file did not come back"

  printf '\033[1;32mE2E PASSED (incremental-fidelity): full then delta restored everything size and mtime cannot see.\033[0m\n'
}

# ── case: offsite-envelope ───────────────────────────────────────────────────

case_offsite_envelope() {
  local DEST_ROOT="/app/backups/e2e-offsite" APP_ID DEST_ID CID BID KEY i
  docker volume create "$VOL" >/dev/null
  docker run -d --name "$TARGET" -v "$VOL":/data alpine sleep 900 >/dev/null
  docker exec "$TARGET" sh -c 'echo offsite-data > /data/file'
  APP_ID="$("${COMPOSE[@]}" ps -q backup-app)"
  HOSTDIR="$(mktemp -d /tmp/dback-e2e-offsite-XXXXXX)"
  # location_field <field> — this destination's entry on the backup, as the catalog has it.
  location_field() {
    curl -fsS -b "$JAR" "$BASE/api/backups/$BID" \
      | jget '[l for l in json.loads(d["locations"]) if l.get("dest_id")=="'"$DEST_ID"'"][0].get("'"$1"'","")'
  }
  # offline_open — try to open the destination's copy with nothing but its own files.
  offline_open() {
    rm -rf "$HOSTDIR/remote" "$HOSTDIR/opened.tar"
    docker cp "$APP_ID:$DEST_ROOT" "$HOSTDIR/remote" >/dev/null 2>&1 || fail "could not copy the destination's files out"
    python3 scripts/dockback-recover.py --key "$DOCKBACK_ENCRYPTION_KEY" --in "$HOSTDIR/remote/$KEY" \
      --out "$HOSTDIR/opened.tar" >/dev/null 2>&1
  }

  step "Back up to an offsite destination"
  DEST_ID="$(curl -fsS "${auth[@]}" -X POST "$BASE/api/destinations" \
    -d "{\"name\":\"e2e-offsite\",\"type\":\"local\",\"config\":{\"path\":\"$DEST_ROOT\"}}" | jget 'd["id"]')" \
    || fail "could not add the destination"
  CID="$(find_container "$TARGET")"
  BID="$(backup_and_verify "$CID" "$TARGET" "$DEST_ID")"
  KEY="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID" | jget 'd["storage_key"]')"
  [ -z "$(location_field status)" ] || fail "the offsite copy should be good: $(location_field detail)"
  offline_open || fail "a complete offsite copy must open from its own files"
  tar -tf "$HOSTDIR/opened.tar" | grep -q '^manifest.json$' || fail "the opened copy holds no manifest"
  echo "  a complete copy opens from the destination's files alone"

  step "Lose the envelope, and show the copy can no longer stand alone"
  docker run --rm --volumes-from "$APP_ID" alpine sh -c "rm -f '$DEST_ROOT/$KEY'.manifest.json*"
  offline_open && fail "the test is void: the copy opened without its envelope"
  echo "  without its envelope the remote archive cannot be opened"

  step "A scrub of that copy finds it"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID/verify" -d "{\"source\":\"$DEST_ID\"}" >/dev/null
  # One read, for status and reason together: the offsite drain may repair the copy
  # moments later, which is its job and is what the next step also accepts.
  local FOUND=""
  for i in $(seq 1 40); do
    FOUND="$(location_field status)|$(location_field detail)"
    case "$FOUND" in incomplete\|*envelope*) break ;; esac
    [ "$i" = 40 ] && fail "the scrub did not mark the copy incomplete and say why: $FOUND"
    sleep 1
  done
  echo "  the copy now reads: $FOUND"

  step "Send offsite repairs it without sending the archive again"
  local BEFORE AFTER
  BEFORE="$(docker run --rm --volumes-from "$APP_ID" alpine stat -c '%i %Y' "$DEST_ROOT/$KEY")"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID/mirror" -d "{\"destinations\":[\"$DEST_ID\"]}" >/dev/null
  for i in $(seq 1 40); do
    [ -z "$(location_field status)" ] && break
    [ "$i" = 40 ] && fail "the repair did not finish: $(location_field detail)"
    sleep 1
  done
  AFTER="$(docker run --rm --volumes-from "$APP_ID" alpine stat -c '%i %Y' "$DEST_ROOT/$KEY")"
  [ "$BEFORE" = "$AFTER" ] || fail "the repair sent the archive again"

  step "The repaired copy opens from the destination's files alone"
  offline_open || fail "the repaired copy must open from its own files"
  tar -tf "$HOSTDIR/opened.tar" | grep -q '^volumes.tar$' || fail "the opened copy holds no volume payload"

  printf '\033[1;32mE2E PASSED (offsite-envelope): a copy that lost its envelope was found, repaired in place, and opened from its own files.\033[0m\n'
}

# ── case: delete-incomplete ──────────────────────────────────────────────────

case_delete_incomplete() {
  local DEST_ROOT="/app/backups/e2e-undeletable" APP_ID DEST_ID CID BID KEY COPY_DIR REPLY CODE
  docker volume create "$VOL" >/dev/null
  docker run -d --name "$TARGET" -v "$VOL":/data alpine sleep 900 >/dev/null
  docker exec "$TARGET" sh -c 'echo delete-data > /data/file'
  APP_ID="$("${COMPOSE[@]}" ps -q backup-app)"
  # in_app_volumes <command> — run a root shell over the app's own volumes.
  in_app_volumes() { docker run --rm --volumes-from "$APP_ID" alpine sh -c "$1"; }
  # backup_field <python expression over d> — one field of the backup's catalog row.
  backup_field() { curl -fsS -b "$JAR" "$BASE/api/backups/$BID" | jget "$1"; }
  # delete_backup — ask for the delete and print the reply.
  delete_backup() { curl -fsS "${auth[@]}" -X DELETE "$BASE/api/backups/$BID"; }

  step "Back up to an offsite destination"
  DEST_ID="$(curl -fsS "${auth[@]}" -X POST "$BASE/api/destinations" \
    -d "{\"name\":\"e2e-undeletable\",\"type\":\"local\",\"config\":{\"path\":\"$DEST_ROOT\"}}" | jget 'd["id"]')" \
    || fail "could not add the destination"
  CID="$(find_container "$TARGET")"
  BID="$(backup_and_verify "$CID" "$TARGET" "$DEST_ID")"
  KEY="$(backup_field 'd["storage_key"]')"
  COPY_DIR="$(dirname "$DEST_ROOT/$KEY")"
  in_app_volumes "test -f '$DEST_ROOT/$KEY' && test -f '/app/backups/$KEY'" || fail "the backup should be stored locally and offsite"

  step "Make the offsite copy impossible to delete, then delete the backup"
  # The app does not run as root, so a root-owned folder it may only read refuses its unlink.
  in_app_volumes "chown root:root '$COPY_DIR' && chmod 555 '$COPY_DIR'"
  REPLY="$(delete_backup)"
  [ "$(printf '%s' "$REPLY" | jget 'd["status"]')" = "incomplete" ] || fail "the delete should answer incomplete: $REPLY"
  [ "$(printf '%s' "$REPLY" | jget 'd["kept"][0]["location"]')" = "e2e-undeletable" ] || fail "the reply should name the copy that is left: $REPLY"
  echo "  the delete answered: $REPLY"

  step "The backup stays listed, holding only what is left"
  [ "$(backup_field 'd["status"]')" = "deleting" ] || fail "the backup should be listed as being deleted"
  [ "$(backup_field 'len(json.loads(d["locations"]))')" = "1" ] || fail "only the copy that is left should be listed"
  [ "$(backup_field 'json.loads(d["locations"])[0].get("delete_outcome","")')" = "failed" ] || fail "the copy should say its delete failed"
  in_app_volumes "test ! -e '/app/backups/$KEY'" || fail "the local copy could be deleted, so it must be gone"
  in_app_volumes "test -f '$DEST_ROOT/$KEY' && test -f '$DEST_ROOT/$KEY.manifest.json'" || fail "the copy that stays must keep its archive and its manifest"

  step "It can no longer be restored"
  CODE="$(curl -sS -o /dev/null -w '%{http_code}' "${auth[@]}" -X POST "$BASE/api/backups/$BID/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"confirm\":true}")"
  [ "$CODE" = "409" ] || fail "restoring a backup that is being deleted should be refused, got $CODE"

  step "Deleting it again while nothing changed leaves it listed"
  [ "$(delete_backup | jget 'd["status"]')" = "incomplete" ] || fail "a retry that cannot finish should still answer incomplete"

  step "Once the destination allows it, the same delete finishes"
  in_app_volumes "chown 65532:65532 '$COPY_DIR' && chmod 755 '$COPY_DIR'"
  [ "$(delete_backup | jget 'd["status"]')" = "deleted" ] || fail "the delete should finish once the copy can be removed"
  CODE="$(curl -sS -o /dev/null -w '%{http_code}' -b "$JAR" "$BASE/api/backups/$BID")"
  [ "$CODE" = "404" ] || fail "the backup should have left the catalog, got $CODE"
  in_app_volumes "test ! -e '$DEST_ROOT/$KEY' && test ! -e '$DEST_ROOT/$KEY.manifest.json'" || fail "the offsite copy and its manifest should be gone"

  printf '\033[1;32mE2E PASSED (delete-incomplete): a copy that could not be deleted kept its backup listed, and the delete finished when it could.\033[0m\n'
}

# ── case: offsite-window ─────────────────────────────────────────────────────

case_offsite_window() {
  local DEST_ROOT="/app/backups/e2e-window" APP_ID DEST_ID CID BID KEY i BEFORE AFTER
  # The drain looks once a minute, so this outlasts one look.
  local DRAIN_LOOK=65
  docker volume create "$VOL" >/dev/null
  docker run -d --name "$TARGET" -v "$VOL":/data alpine sleep 900 >/dev/null
  docker exec "$TARGET" sh -c 'echo window-data > /data/file'
  APP_ID="$("${COMPOSE[@]}" ps -q backup-app)"
  location_status() {
    curl -fsS -b "$JAR" "$BASE/api/backups/$BID" \
      | jget '[l for l in json.loads(d["locations"]) if l.get("dest_id")=="'"$DEST_ID"'"][0].get("status","")'
  }
  remote_archive() {
    docker run --rm --volumes-from "$APP_ID" alpine stat -c '%i %Y' "$DEST_ROOT/$KEY" 2>/dev/null || true
  }

  step "Back up with the destination's upload window closed"
  # The app runs in UTC. A window two hours from now cannot be open during this case.
  DEST_ID="$(curl -fsS "${auth[@]}" -X POST "$BASE/api/destinations" \
    -d "{\"name\":\"e2e-window\",\"type\":\"local\",\"config\":{\"path\":\"$DEST_ROOT\",\"upload_window_start\":\"$(date -u -d '+2 hours' +%H:%M)\",\"upload_window_end\":\"$(date -u -d '+3 hours' +%H:%M)\"}}" \
    | jget 'd["id"]')" || fail "could not add the destination"
  CID="$(find_container "$TARGET")"
  BID="$(backup_and_verify "$CID" "$TARGET" "$DEST_ID")"
  KEY="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID" | jget 'd["storage_key"]')"
  [ "$(location_status)" = deferred ] || fail "outside its window the copy should be deferred"

  step "Restart DockBack, and let the drain look at it with the window still closed"
  "${COMPOSE[@]}" restart backup-app >/dev/null 2>&1
  log_in
  sleep "$DRAIN_LOOK"
  [ "$(location_status)" = deferred ] || fail "a closed window must keep the copy deferred"
  [ -z "$(remote_archive)" ] || fail "the archive was sent outside the window"

  step "Open the window. No new backup, and no one asks"
  curl -fsS "${auth[@]}" -X PUT "$BASE/api/destinations/$DEST_ID" \
    -d "{\"name\":\"e2e-window\",\"type\":\"local\",\"config\":{\"upload_window_start\":\"\",\"upload_window_end\":\"\"}}" >/dev/null \
    || fail "could not open the window"
  for i in $(seq 1 45); do
    [ -z "$(location_status)" ] && break
    [ "$i" = 45 ] && fail "the copy was not sent after the window opened: $(location_status)"
    sleep 3
  done
  BEFORE="$(remote_archive)"
  [ -n "$BEFORE" ] || fail "the copy reads as good with no archive on the destination"

  step "It goes only once"
  sleep "$DRAIN_LOOK"
  AFTER="$(remote_archive)"
  [ "$AFTER" = "$BEFORE" ] || fail "the archive was sent again: $BEFORE then $AFTER"

  printf '\033[1;32mE2E PASSED (offsite-window): a deferred copy survived a restart and went by itself, once, when its window opened.\033[0m\n'
}

# ── case: restore-outcome ────────────────────────────────────────────────────

case_restore_outcome() {
  local APP="$PROJECT-backup-app-1" CID BID i
  local FIRST="" REPEAT="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" KILLED="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  docker volume create "$VOL" >/dev/null
  docker run -d --name "$TARGET" -v "$VOL":/data alpine sleep 900 >/dev/null
  docker exec "$TARGET" sh -c 'echo outcome-data > /data/file'
  CID="$(find_container "$TARGET")"
  # status_field <field> [execution] — what the record says.
  status_field() {
    curl -fsS -b "$JAR" "$BASE/api/restores/$BID/status${2:+?execution=$2}" | jget 'd.get("'"$1"'","")'
  }
  start_restore() {
    curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID/restore" \
      -d "{\"execution_id\":\"$1\",\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"database\":false,\"confirm\":true}" >/dev/null
  }
  await_outcome() {
    for i in $(seq 1 90); do
      local outcome
      outcome="$(status_field outcome "$1" 2>/dev/null || true)"
      case "$outcome" in ""|running) ;; *) printf '%s' "$outcome"; return 0 ;; esac
      sleep 2
    done
    fail "the restore never reached an outcome"
  }

  step "A post-restore hook that fails, then a restore"
  curl -fsS "${auth[@]}" -X PUT "$BASE/api/nodes/local/containers/$CID/hooks" \
    -d "{\"post_restore\":[\"sh -c \\\"exit 7\\\"\"],$CONFIRM_PASSWORD}" >/dev/null || fail "could not set the hook"
  BID="$(backup_and_verify "$CID" "$TARGET")"
  start_restore ""
  [ "$(await_outcome "")" = ok ] || fail "the hook is non-fatal, so the restore should have succeeded: $(status_field message)"
  local LOG
  LOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log")"
  case "$LOG" in
    *"post-restore-hook failed"*) ;;
    *) printf '%s\n' "$LOG" | tail -c 2500 >&2; fail "the hook did not log its failure, so this proves nothing" ;;
  esac
  FIRST="$(status_field execution_id)"
  echo "  the log has the hook's failure, and the record says ok"

  step "A repeat restore of the same backup keeps its own record"
  start_restore "$REPEAT"
  [ "$(await_outcome "$REPEAT")" = ok ] || fail "the repeat restore failed: $(status_field message "$REPEAT")"
  [ "$(status_field execution_id)" = "$REPEAT" ] || fail "the newest record should be the repeat"
  [ "$(status_field outcome "$FIRST")" = ok ] || fail "the first run lost its record"

  step "A restore cut off by SIGKILL reads as interrupted after the restart"
  start_restore "$KILLED"
  docker kill -s KILL "$APP" >/dev/null
  docker start "$APP" >/dev/null
  log_in
  [ "$(status_field outcome "$KILLED")" = interrupted ] \
    || fail "a restore the process died during must not read as running or fine: $(status_field outcome "$KILLED")"
  [ "$(status_field outcome "$REPEAT")" = ok ] || fail "a finished run must survive the restart"

  printf '\033[1;32mE2E PASSED (restore-outcome): the record, not the log, says how each restore ended, across a repeat and a SIGKILL.\033[0m\n'
}

# ── case: clone-redaction ────────────────────────────────────────────────────

case_clone_redaction() {
  local COPY="$TARGET-copy" IMAGE="dback-e2e/linuxserver/plex:$SUFFIX" TOKEN="invented-token-$SUFFIX"
  local DIR="/config/Library/Application Support/Plex Media Server" CID BID i
  EXTRA_CONTAINERS+=("$COPY")
  docker pull -q alpine:3.20 >/dev/null
  docker tag alpine:3.20 "$IMAGE"
  docker volume create "$VOL" >/dev/null

  step "An application that records, on every start, what its config file said"
  # What the first start saw is the proof the file was clean before the application ran.
  docker run -d --name "$TARGET" -v "$VOL":/config "$IMAGE" sh -c \
    'mkdir -p "$0" && [ -f "$0/Preferences.xml" ] && cp "$0/Preferences.xml" /config/seen-at-start; sleep 900' "$DIR" >/dev/null
  docker exec "$TARGET" sh -c "mkdir -p '$DIR' && printf '<Preferences MachineIdentifier=\"keep-me\" PlexOnlineToken=\"$TOKEN\" PublishServerOnPlexOnlineKey=\"1\"/>' > '$DIR/Preferences.xml' && chown 1000:1000 '$DIR/Preferences.xml' && chmod 640 '$DIR/Preferences.xml'"
  CID="$(find_container "$TARGET")"
  BID="$(backup_and_verify "$CID" "$TARGET")"

  step "Restore it as a copy"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$BID/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$CID\",\"volumes\":true,\"database\":false,\"confirm\":true,\"as_name\":\"$COPY\",\"isolated\":true}" >/dev/null
  for i in $(seq 1 40); do
    [ "$(docker inspect -f '{{.State.Running}}' "$COPY" 2>/dev/null || true)" = "true" ] && break
    [ "$i" = 40 ] && { curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log" | tail -c 1200 >&2; fail "the copy never came up"; }
    sleep 3
  done
  sleep 2

  step "The copy's first start never saw the token"
  local SEEN NOW
  SEEN="$(docker exec "$COPY" cat /config/seen-at-start)" || fail "the copy did not record its first start"
  NOW="$(docker exec "$COPY" cat "$DIR/Preferences.xml")"
  case "$SEEN" in *"$TOKEN"*) fail "the application started with the token in its file" ;; esac
  case "$SEEN" in *'PlexOnlineToken=""'*'PublishServerOnPlexOnlineKey=""'*) ;; *) fail "the values should be emptied: $SEEN" ;; esac
  case "$NOW" in *'MachineIdentifier="keep-me"'*) ;; *) fail "other settings must survive" ;; esac
  [ "$(docker exec "$COPY" stat -c '%u:%g %a' "$DIR/Preferences.xml")" = "1000:1000 640" ] \
    || fail "the file must keep its owner and mode"
  [ -z "$(docker exec "$COPY" sh -c "ls -a '$DIR' | grep dockback || true")" ] || fail "working copies were left behind"

  step "The original keeps its token, and the log does not hold it"
  case "$(docker exec "$TARGET" cat "$DIR/Preferences.xml")" in *"$TOKEN"*) ;; *) fail "the original was changed" ;; esac
  local LOG
  LOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/log")"
  case "$LOG" in *"$TOKEN"*) fail "the token reached the log" ;; esac

  docker rm -f "$COPY" "$TARGET" >/dev/null 2>&1 || true
  docker image rm "$IMAGE" >/dev/null 2>&1 || true
  printf '\033[1;32mE2E PASSED (clone-redaction): the copy came up with its token gone before its first start, and the original kept its own.\033[0m\n'
}

# ── case: stack-parity ───────────────────────────────────────────────────────

case_stack_parity() {
  local PROJ="dback-e2e-parity-$SUFFIX" IMAGE="dback-e2e/mealie:$SUFFIX" CID BID i
  EXTRA_PROJECTS+=("$PROJ")
  HOSTDIR="$(mktemp -d /tmp/dback-e2e-parity-XXXXXX)"
  docker pull -q alpine:3.20 >/dev/null
  # A profile's image name on alpine, so its declared exclusions apply.
  docker tag alpine:3.20 "$IMAGE"
  cat > "$HOSTDIR/docker-compose.yml" <<EOF
services:
  app:
    image: $IMAGE
    command: ["sleep", "900"]
    volumes: [app-data:/app/data]
  other:
    image: alpine:3.20
    command: ["sleep", "900"]
    volumes: [other-data:/data]
volumes:
  app-data:
  other-data:
EOF
  docker compose -p "$PROJ" --project-directory "$HOSTDIR" -f "$HOSTDIR/docker-compose.yml" up -d --quiet-pull >/dev/null 2>&1 \
    || fail "the test stack did not start"
  docker exec "$PROJ-app-1" sh -c 'echo recipe > /app/data/recipe.txt; echo noise > /app/data/mealie.log.1; mkdir -p /app/data/.temp; echo scratch > /app/data/.temp/x'
  CID="$(find_container "$PROJ-app-1")"

  step "Save this service's own choices: its image, an app-native export, incremental"
  curl -fsS "${auth[@]}" -X PUT "$BASE/api/nodes/local/containers/$CID/backup-options" \
    -d '{"compression":"balanced","save_image":true,"app_export":true,"incremental":true}' >/dev/null || fail "could not save the options"
  curl -fsS "${auth[@]}" -X PUT "$BASE/api/nodes/local/containers/$CID/export-profile" \
    -d "{\"dir\":\"/app/export\",\"export_cmd\":\"mkdir -p /app/export && cp /app/data/recipe.txt /app/export/\",\"import_cmd\":\"true\",$CONFIRM_PASSWORD}" >/dev/null \
    || fail "could not save the export profile"

  step "Take an app-consistent snapshot of the stack"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/nodes/local/stacks/$PROJ/backup" -d '{"consistent":true}' >/dev/null
  local SLOG=""
  for i in $(seq 1 90); do
    SLOG="$(curl -fsS -b "$JAR" "$BASE/api/backups/stack:$PROJ/log" || true)"
    case "$SLOG" in
      *"App-consistent snapshot complete"*) break ;;
      *"App-consistent snapshot failed"*) fail "the snapshot failed" ;;
    esac
    [ "$i" = 90 ] && fail "the snapshot never finished"
    sleep 2
  done
  BID="$(curl -fsS -b "$JAR" "$BASE/api/backups?node_id=local&page_size=50" \
    | jget '[b for b in d["items"] if b["target_name"]=="'"$PROJ-app-1"'" and b["status"]=="success"][0]["id"]')"
  local MAN
  MAN="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID" | jget 'd["manifest_json"]')"
  manifest() { printf '%s' "$MAN" | python3 -c "import sys,json; m=json.load(sys.stdin); print($1)"; }

  step "Its archive holds what its own settings asked for"
  [ "$(manifest 'm.get("consistency_group","")!=""')" = True ] || fail "this is not a group snapshot, so it proves nothing"
  [ "$(manifest 'bool(m.get("image_tar"))')" = True ] || fail "the saved image was not bundled"
  [ "$(manifest 'm.get("vol_index","")!=""')" = True ] || fail "the file index was not written"
  [ "$(manifest 'sorted(p["path"] for p in m.get("excluded_regenerable") or [])')" = "['/app/data/.temp', '/app/data/mealie.log*']" ] \
    || fail "the paths this profile never keeps were not recorded as left out: $(manifest 'm.get("excluded_regenerable")')"
  local ENTRIES
  ENTRIES="$(curl -fsS -b "$JAR" "$BASE/api/backups/$BID/entries" | jget '" ".join(e["name"] for e in d["entries"])')"
  case "$ENTRIES" in *recipe.txt*) ;; *) fail "the kept file is missing: $ENTRIES" ;; esac
  case "$ENTRIES" in *mealie.log.1*|*.temp/x*) fail "a path with no restore value was archived: $ENTRIES" ;; esac
  echo "  image bundled, index written, the two left-out paths absent"

  step "What a group cannot honour is on the archive, not dropped"
  [ "$(manifest 'sorted(f["subject"] for f in m.get("findings") or [] if f["code"]=="capture-overridden")')" = "['app-native export', 'incremental']" ] \
    || fail "the overrides were not recorded: $(manifest '[f for f in m.get("findings") or []]')"
  [ "$(manifest 'm.get("app_export") is None and not m.get("incremental", False)')" = True ] || fail "the archive claims what it did not do"

  docker compose -p "$PROJ" --project-directory "$HOSTDIR" -f "$HOSTDIR/docker-compose.yml" down -v >/dev/null 2>&1 || true
  docker image rm "$IMAGE" >/dev/null 2>&1 || true
  printf '\033[1;32mE2E PASSED (stack-parity): a stack snapshot kept every saved choice it could, and said which it could not.\033[0m\n'
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

  HOSTDIR="$(mktemp -d /tmp/dback-e2e-pg-XXXXXX)"
  mkdir -p "$HOSTDIR/src-init" "$HOSTDIR/dst-init"
  printf 'select 1;\n' > "$HOSTDIR/src-init/01-init.sql"

  step "Stand up a source PostgreSQL (no TZ), with an init-scripts mount, and seed it"
  docker run -d --name "$SRC" -e POSTGRES_PASSWORD=e2e -v "$HOSTDIR/src-init":/docker-entrypoint-initdb.d:ro postgres:16-alpine >/dev/null
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
  docker run -d --name "$DST" -e POSTGRES_PASSWORD=e2e -v "$HOSTDIR/dst-init":/docker-entrypoint-initdb.d postgres:16-alpine >/dev/null
  for i in $(seq 1 60); do docker exec "$DST" pg_isready -q 2>/dev/null && break; [ "$i" = 60 ] && fail "target postgres never became ready"; sleep 1; done
  DST_CID="$(find_container "$DST")"
  curl -fsS "${auth[@]}" -X POST "$BASE/api/backups/$SRC_BID/restore" \
    -d "{\"node_id\":\"local\",\"target_id\":\"$DST_CID\",\"volumes\":true,\"database\":true,\"confirm\":true,\"confirm_incompatible\":true}" >/dev/null

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

  step "Assert the database container's other mount came back (step 24)"
  # A restore from the dump used to bring back the data and nothing else: the
  # init-scripts and configuration mounts stayed empty.
  for i in $(seq 1 20); do [ -f "$HOSTDIR/dst-init/01-init.sql" ] && break; sleep 3; done
  [ -f "$HOSTDIR/dst-init/01-init.sql" ] || fail "the init-scripts mount was not restored alongside the dump"
  echo "  init-scripts mount restored"

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
  deleted-binds) case_deleted_binds ;;
  moved-bind) case_moved_bind ;;
  stack-folder) case_stack_folder ;;
  files-only) case_files_only ;;
  stack-selection) case_stack_selection ;;
  clone-ownership) case_clone_ownership ;;
  clone-sandbox) case_clone_sandbox ;;
  revert-rollback) case_revert_rollback ;;
  shutdown-holds) case_shutdown_holds ;;
  hook-cleanup) case_hook_cleanup ;;
  hold-required) case_hold_required ;;
  incremental-fidelity) case_incremental_fidelity ;;
  offsite-envelope) case_offsite_envelope ;;
  delete-incomplete) case_delete_incomplete ;;
  offsite-window) case_offsite_window ;;
  restore-outcome) case_restore_outcome ;;
  clone-redaction) case_clone_redaction ;;
  stack-parity) case_stack_parity ;;
  *) fail "unknown E2E_CASE '$E2E_CASE' (known: delete-incomplete, offsite-envelope, offsite-window, restore-outcome, clone-redaction, stack-parity, sentinel, pg-hash, deleted-binds, moved-bind, stack-folder, files-only, stack-selection, clone-ownership, clone-sandbox, revert-rollback, shutdown-holds, hook-cleanup, hold-required, incremental-fidelity)" ;;
esac
