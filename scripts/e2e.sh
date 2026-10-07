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
# Five cases, selected with E2E_CASE:
#
#   sentinel (default) — the drill above: volume data lost and brought back.
#   deleted-binds      — the recovery of 2026-10-04: a RUNNING container whose
#                        folder and secret-file binds are read-only has its whole
#                        host folder deleted, then is restored IN PLACE. Both must
#                        come back — the secret as a real file, mode 600, not the
#                        empty directory Docker invents for a missing file bind.
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
#   pg-hash            — #18's content baseline: a PostgreSQL database with a
#                        timestamptz column is backed up, restored into another
#                        container, and the restored cluster is then put in a
#                        DIFFERENT TimeZone before being hashed again. R4 §31
#                        made seven false table mismatches out of eight that way;
#                        the hashes must come back identical.
#
#   ./scripts/e2e.sh
#   E2E_CASE=pg-hash ./scripts/e2e.sh
#   E2E_CASE=deleted-binds ./scripts/e2e.sh
#   E2E_CASE=stack-folder ./scripts/e2e.sh
#   E2E_CASE=files-only ./scripts/e2e.sh
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

  printf '\033[1;32mE2E PASSED (files-only): the files came back with the container never stopped, and the missing data was named.\033[0m\n'
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
  stack-folder) case_stack_folder ;;
  files-only) case_files_only ;;
  *) fail "unknown E2E_CASE '$E2E_CASE' (known: sentinel, pg-hash, deleted-binds, stack-folder, files-only)" ;;
esac
