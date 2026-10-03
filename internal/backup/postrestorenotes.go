package backup

import (
	"context"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Post-restore notes from the application's own database (F170).
//
// A restore can be complete and correct and still leave work to do, because some
// applications depend on things a database cannot bring back.
//
// Wiki.js is the case that motivates it. With its default settings a restore is
// genuinely finished: pages, history, users, permissions and even the uploaded
// attachments all live in the database, so importing it restores everything.
// Turn on an external search engine, though, and the index lives in that engine
// rather than in the backup — the wiki comes back complete and searching finds
// nothing until somebody rebuilds it. Add a Git mirror and its remote and deploy
// key want checking before it starts syncing.
//
// Neither is visible from the image, the manifest, or anything else DockBack can
// see from outside. Only the application's own configuration knows, and after a
// restore that configuration is sitting in the database that was just imported.
//
// Two things make this awkward, and both shape the design:
//
//   - The profile belongs to the APPLICATION and the database lives in a
//     DIFFERENT container. So this runs at the end of a STACK restore, where
//     both are in hand, rather than on either service's own restore.
//   - The application's database is one of several in the cluster, and its name
//     is a deployment choice. So the query runs against every connectable
//     database and the answers are summed — the same walk the Postgres sanity
//     check already does, for the same reason. A query referencing a table that
//     does not exist in some other database simply contributes nothing.
//
// Read-only throughout: a SELECT, and a sentence. Nothing here writes, and
// nothing here can fail a restore, because nothing here is a statement about
// whether the restore worked.

// maxNoteRows bounds what counts as "yes" before the loop stops caring. The
// queries return one row per active thing; a query returning thousands has gone
// wrong, and the note is one sentence either way.
const maxNoteRows = 200

// postRestoreNotesFor returns the notes an image's application declares.
func postRestoreNotesFor(image string) []PostRestoreNote {
	p := ProfileFor(image)
	if p == nil {
		return nil
	}
	return p.PostRestoreNotes
}

// noteQueryCmd asks one note's question of every database in the server,
// printing a single total.
//
// The count is all that crosses back, deliberately: the query runs against a
// database holding user accounts and application secrets, and the note needs to
// know only whether something is switched on. Returning rows would mean deciding
// what is safe to print from tables DockBack does not own.
func noteQueryCmd(engine, sql string) []string {
	wrapped := "SELECT count(*) FROM (" + sql + ") AS dockback_note"
	switch engine {
	case "postgres":
		return []string{"/bin/sh", "-c",
			`command -v psql >/dev/null 2>&1 || exit 0; ` +
				`U="${POSTGRES_USER:-postgres}"; T=0; ` +
				`for db in $(psql -h 127.0.0.1 -tAqX -U "$U" -d postgres -c "SELECT datname FROM pg_database WHERE datallowconn AND datname NOT IN ('template0','template1')" 2>/dev/null); do ` +
				`n=$(psql -h 127.0.0.1 -tAqX -U "$U" -d "$db" -c '` + shellEscape(wrapped) + `' 2>/dev/null); ` +
				`case "$n" in ''|*[!0-9]*) n=0;; esac; T=$((T+n)); done; printf '%s\n' "$T"`}
	case "mysql":
		return []string{"/bin/sh", "-c",
			`CLI=mysql; command -v mariadb >/dev/null 2>&1 && CLI=mariadb; ` +
				`command -v "$CLI" >/dev/null 2>&1 || exit 0; T=0; ` +
				`for db in $("$CLI" -N -B -e "SHOW DATABASES" 2>/dev/null | grep -Ev '^(information_schema|performance_schema|mysql|sys)$'); do ` +
				`n=$("$CLI" -N -B -e '` + shellEscape(wrapped) + `' "$db" 2>/dev/null); ` +
				`case "$n" in ''|*[!0-9]*) n=0;; esac; T=$((T+n)); done; printf '%s\n' "$T"`}
	}
	return nil
}

// reportPostRestoreNotes asks the restored database which optional subsystems
// are active and says what each one needs (F170).
//
// appImage identifies whose notes to ask; dbContainer and engine say where to
// ask them. Best-effort throughout: a query that cannot run, a database not
// there yet, or an application using the defaults all produce silence, which is
// the honest outcome for a question that could not be answered.
func (e *Engine) reportPostRestoreNotes(ctx context.Context, cli *client.Client, logID, appImage, dbContainer, engine string) {
	notes := postRestoreNotesFor(appImage)
	if len(notes) == 0 || dbContainer == "" || engine == "" {
		return
	}
	for _, n := range notes {
		if n.Engine != "" && n.Engine != engine {
			continue
		}
		cmd := noteQueryCmd(engine, n.SQL)
		if cmd == nil {
			continue
		}
		out, err := dockercli.ExecCapture(ctx, cli, dbContainer, cmd)
		if err != nil {
			continue // could not ask; claim nothing
		}
		if noteApplies(string(out)) {
			e.logf(logID, "WARN", "The data is restored, but one thing it depends on is not in the backup: %s", n.Note)
		}
	}
}

// noteApplies reads the query's total. Anything unreadable is "no": a note is a
// prompt to go and do work, and inventing one from an answer that could not be
// parsed sends somebody to do work that is not needed.
func noteApplies(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n := 0
		for _, r := range line {
			if r < '0' || r > '9' {
				return false
			}
			n = n*10 + int(r-'0')
			if n > maxNoteRows {
				return true
			}
		}
		return n > 0
	}
	return false
}
