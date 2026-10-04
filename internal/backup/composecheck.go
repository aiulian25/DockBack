package backup

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
)

// Every compose file a restore writes is run through Docker Compose (step 15).
// A reconstruction Compose rejects never takes the stack's compose name; a file
// that is written is reported valid or not, with what Compose warned about and
// whether `docker compose up -d` would change anything. In the 2026-10-04 recovery
// that check was done by hand for every stack.

// openComposeChecker borrows the Compose CLI image for one restore, or says
// why the files go unchecked this time. Nil means unchecked.
func (e *Engine) openComposeChecker(ctx context.Context, cli *client.Client, logID string) *dockercli.ComposeChecker {
	checker, err := dockercli.OpenComposeChecker(ctx, cli)
	if err != nil {
		e.logf(logID, "INFO", "The compose files are not checked with Docker Compose this time: %v", err)
		return nil
	}
	return checker
}

// guardReconstruction checks a reconstruction that is about to become the
// stack's compose file because the backup holds no original. One that Compose
// rejects goes beside the folder instead, under a name Compose ignores.
func (e *Engine) guardReconstruction(ctx context.Context, checker *dockercli.ComposeChecker, layout stackFolderLayout, envFile []byte, project, logID string) stackFolderLayout {
	if checker == nil || layout.PrimaryOriginal || len(layout.Primary) == 0 {
		return layout
	}
	check, err := checker.CheckFiles(ctx, layout.Primary, envFile, project)
	if err != nil {
		e.logf(logID, "INFO", "The rebuilt compose file was not checked with Docker Compose: %v", err)
		return layout
	}
	if check.Valid {
		return layout
	}
	e.logf(logID, "WARN", "Docker Compose rejects the rebuilt compose file (%s), so it is not written as the stack's compose file. It goes beside it as %s for you to fix.", check.Problem, reconstructionComposeName)
	return asideInvalidReconstruction(layout)
}

// asideInvalidReconstruction moves a rejected reconstruction off the
// canonical name and beside the folder's other files. Pure.
func asideInvalidReconstruction(layout stackFolderLayout) stackFolderLayout {
	return stackFolderLayout{Beside: append([]dockercli.NamedFile{{Name: reconstructionComposeName, Content: layout.Primary}}, layout.Beside...)}
}

// reportComposeCheck runs the written compose files through Docker Compose
// where they now sit, once the project's other files are back so env_file
// entries resolve. compareRunning is false for a container no Compose project
// created: there is no configuration hash to compare against.
func (e *Engine) reportComposeCheck(ctx context.Context, cli *client.Client, checker *dockercli.ComposeChecker, res *dockercli.HostReconstructResult, layout stackFolderLayout, project string, compareRunning bool, logID string) {
	if checker == nil || res == nil {
		return
	}
	if res.Path != "" {
		file := path.Base(res.Path)
		check, err := checker.CheckFolder(ctx, res.Dir, file, project)
		if err != nil {
			e.logf(logID, "INFO", "%s was not checked with Docker Compose: %v", file, err)
		} else {
			var running map[string]string
			if compareRunning {
				running, _ = dockercli.RunningConfigHashes(ctx, cli, project)
				if running == nil {
					running = map[string]string{}
				}
			}
			level, message := composeCheckSummary(file, check, running)
			e.logf(logID, level, "%s", message)
		}
	}
	if !layout.PrimaryOriginal || !slices.Contains(res.Beside, path.Join(res.Dir, reconstructionComposeName)) {
		return
	}
	if check, err := checker.CheckFolder(ctx, res.Dir, reconstructionComposeName, project); err == nil && !check.Valid {
		e.logf(logID, "WARN", "The reconstruction beside it, %s, is not valid for Docker Compose: %s", reconstructionComposeName, check.Problem)
	}
}

// composeCheckSummary is the restore log's one-line answer for a checked file.
// running maps each service to the configuration hash its container was
// created from; nil means there is nothing to compare, so only validity is
// stated. Pure.
func composeCheckSummary(file string, check *dockercli.ComposeCheck, running map[string]string) (level, message string) {
	by := "Docker Compose"
	if check.Version != "" {
		by += " " + check.Version
	}
	if !check.Valid {
		return "WARN", fmt.Sprintf("Checked with %s: %s is NOT valid — %s. Fix it before `docker compose up`.", by, file, check.Problem)
	}
	warned, level := "", "INFO"
	if len(check.Warnings) > 0 {
		warned, level = " Compose warned: "+strings.Join(check.Warnings, "; "), "WARN"
	}
	if running == nil {
		return level, fmt.Sprintf("Checked with %s: %s is valid.%s", by, file, warned)
	}
	var recreate, create []string
	for _, service := range slices.Sorted(maps.Keys(check.Hashes)) {
		current, exists := running[service]
		switch {
		case !exists:
			create = append(create, service)
		case current != check.Hashes[service]:
			recreate = append(recreate, service)
		}
	}
	if len(recreate) == 0 && len(create) == 0 {
		return level, fmt.Sprintf("Checked with %s: %s is valid, and `docker compose up -d` will change nothing — every service matches its running container.%s", by, file, warned)
	}
	var would []string
	if len(recreate) > 0 {
		would = append(would, "recreate "+strings.Join(recreate, ", "))
	}
	if len(create) > 0 {
		would = append(would, "create "+strings.Join(create, ", ")+" (no container yet)")
	}
	return "WARN", fmt.Sprintf("Checked with %s: %s is valid, but `docker compose up -d` would %s.%s", by, file, strings.Join(would, " and "), warned)
}

// composeProjectName makes a name Compose accepts as a project name: lowercase
// letters, digits, dashes and underscores, starting with a letter or digit.
// For a container no Compose project created. Pure.
func composeProjectName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if out := strings.TrimLeft(b.String(), "-_"); out != "" {
		return out
	}
	return fallbackComposeProject
}

// fallbackComposeProject names a project whose container name has nothing usable.
const fallbackComposeProject = "dockback"
