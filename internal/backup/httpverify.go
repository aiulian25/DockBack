package backup

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/client"

	"dockback/internal/dockercli"
	"dockback/internal/store"
)

// Verifying that a restored application is actually USABLE, not merely up
// (#10, #33).
//
// Two false positives, both measured, both worse than a plain failure.
//
// R2 §4.1: BookStack builds every redirect from APP_URL rather than the Host
// header, so the restored copy answers `302 → https://bookstack.<domain>/login`.
// The operator opens the new host, the browser follows, and they are looking at
// THE ORIGINAL — "a working BookStack, concludes the migration worked, and signs
// off on a restore they never actually tested."
//
// R4 §Issue 33: Paperless renders its login page perfectly and rejects the login
// POST with 403, because Django validates Origin on unsafe methods and the new
// host is not in CSRF_TRUSTED_ORIGINS. "Neither the loud lockout of Nextcloud's
// trusted_domains nor the silent redirect of APP_URL" — a partial failure that
// presents as "the app works but my password is broken".
//
// So: a request that does NOT follow the redirect, and a round-trip POST.

// The probe speaks raw HTTP over nc rather than using a client.
//
// This is not a stylistic choice. busybox wget — the only HTTP client the
// sidecar image has, curl is absent — ALWAYS follows redirects, and following
// this particular redirect means issuing a real request to the operator's live
// production host. Measured: against a container answering
// `302 → https://bookstack.example.com/login`, wget went on to connect to
// `bookstack.example.com (203.0.113.109:443)` and reported 200. That is R2's
// false positive reproduced by the tool meant to catch it, plus an outbound
// request nobody asked for.
//
// One request, one response, no client cleverness.

// httpProbeTimeoutSeconds bounds each request. The app has already passed its
// health gate, so a slow answer here is a finding's absence, not a reason to wait.
const httpProbeTimeoutSeconds = 8

// rawRequestScript builds a single HTTP/1.0 request and reads the reply.
//
// HTTP/1.0 with an explicit Connection: close so the server ends the response
// rather than holding the socket open for a keep-alive that nc will not use.
func rawRequestScript(port int, method, path string, headers []string, body string) string {
	req := method + " " + path + " HTTP/1.0\\r\\n" +
		"Host: 127.0.0.1\\r\\n" +
		"User-Agent: DockBack-restore-check\\r\\n" +
		"Connection: close\\r\\n"
	for _, h := range headers {
		req += h + "\\r\\n"
	}
	if body != "" {
		req += "Content-Type: application/x-www-form-urlencoded\\r\\n" +
			"Content-Length: " + strconv.Itoa(len(body)) + "\\r\\n"
	}
	req += "\\r\\n" + strings.ReplaceAll(body, "\n", "")
	return "printf '%s' '" + shellEscape(req) + "' | nc -w " + strconv.Itoa(httpProbeTimeoutSeconds) + " 127.0.0.1 " + strconv.Itoa(port)
}

// httpResponse is what one probe learned.
type httpResponse struct {
	Status   int
	Location string
	Body     string
	Cookies  []string
}

// parseHTTPResponse reads a raw HTTP reply. Header names are matched
// case-insensitively, because a server may spell them however it likes.
func parseHTTPResponse(raw string) (httpResponse, bool) {
	head, body, split := strings.Cut(strings.ReplaceAll(raw, "\r\n", "\n"), "\n\n")
	if !split {
		head = raw
	}
	lines := strings.Split(head, "\n")
	if len(lines) == 0 {
		return httpResponse{}, false
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 || !strings.HasPrefix(strings.ToUpper(fields[0]), "HTTP/") {
		return httpResponse{}, false
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return httpResponse{}, false
	}
	resp := httpResponse{Status: status, Body: body}
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "location":
			resp.Location = value
		case "set-cookie":
			resp.Cookies = append(resp.Cookies, strings.SplitN(value, ";", 2)[0])
		}
	}
	return resp, true
}

// redirectVerdict decides whether a redirect sends the operator somewhere other
// than the container they are testing.
//
// A relative Location, or one pointing at an address the restore itself is
// about, is fine — that is the application routing within itself. A Location on
// any OTHER host is the failure: whoever follows it leaves the restored copy
// without noticing.
//
// Pure, and it never follows anything.
func redirectVerdict(status int, location string, ownAddrs []string) (offHost bool, host string) {
	if status < 300 || status > 399 || strings.TrimSpace(location) == "" {
		return false, ""
	}
	parsed, err := url.Parse(strings.TrimSpace(location))
	if err != nil || parsed.Host == "" {
		return false, "" // relative: the app routing within itself
	}
	target := strings.ToLower(parsed.Hostname())
	for _, own := range ownAddrs {
		if own = strings.ToLower(strings.TrimSpace(own)); own != "" && own == target {
			return false, target
		}
	}
	return true, target
}

// ownAddressesFor is what "this container" legitimately answers to: its loopback
// names, whatever address the restore was told the application now lives at, and
// its own container name.
func ownAddressesFor(newSiteAddress, containerName string) []string {
	own := []string{"127.0.0.1", "localhost", "[::1]", "::1"}
	if name := strings.TrimSpace(containerName); name != "" {
		own = append(own, strings.TrimPrefix(name, "/"))
	}
	addr := strings.TrimSpace(newSiteAddress)
	if addr == "" {
		return own
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	if parsed, err := url.Parse(addr); err == nil && parsed.Hostname() != "" {
		own = append(own, parsed.Hostname())
	}
	return own
}

// csrfVerdict reads the round-trip POST. R4 §Issue 33's exact discriminator:
// "HTTP 200 (form re-rendered = CSRF accepted); HTTP 403 would mean CSRF
// rejected."
//
// Only 403 is a verdict. Anything else — including a 5xx from an application
// that dislikes the request for its own reasons — is not evidence that origin
// validation rejected it, and inventing one would be the false positive this
// step exists to remove.
func csrfVerdict(status int) (rejected bool) { return status == 403 }

// extractFormToken pulls the anti-forgery token out of a rendered form.
//
// The pattern comes from the application's profile, because the field name is
// the application's: Django writes csrfmiddlewaretoken, and guessing it wrong
// would POST without a token, get the 403 that means "no token" and report it as
// "origin rejected" — a false danger finding about the thing being tested.
func extractFormToken(body, pattern string) (string, bool) {
	if pattern == "" || body == "" {
		return "", false
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", false
	}
	m := re.FindStringSubmatch(body)
	if len(m) < 2 || m[1] == "" {
		return "", false
	}
	return m[1], true
}

// probeUsername is the account the round-trip POST pretends to be.
//
// Deliberately wrong, per R4's method, and deliberately RECOGNISABLE: this lands
// in the application's authentication log, and an operator reading it later
// should see what it was rather than wonder who tried to log in.
const probeUsername = "dockback-restore-check"

// loginFormBody builds the POST body: the extracted token, plus credentials that
// cannot succeed.
func loginFormBody(tokenField, token string) string {
	return url.Values{
		tokenField: {token},
		"username": {probeUsername},
		"password": {"not-a-real-password"},
	}.Encode()
}

// describeRedirect renders the finding's subject line.
func describeRedirect(host, location string) string {
	return fmt.Sprintf("%s (Location: %s)", host, location)
}

// httpProbePort finds the port the application listens on inside its container.
//
// From the container itself, not from a profile: the port is a deployment
// choice, and an image that can be configured would make a hardcoded one wrong
// exactly where it mattered.
func httpProbePort(spec *HTTPVerifySpec, exposed []string) int {
	if spec != nil && spec.Port > 0 {
		return spec.Port
	}
	best := 0
	for _, p := range exposed {
		port, proto, ok := strings.Cut(p, "/")
		if ok && !strings.EqualFold(proto, "tcp") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(port))
		if err != nil || n <= 0 {
			continue
		}
		// Lowest wins, deterministically: an app that exposes both a web port and
		// a metrics one almost always lists the web one lower, and a stable choice
		// matters more than a clever one.
		if best == 0 || n < best {
			best = n
		}
	}
	return best
}

// verifyRestoredHTTP asks the restored application the two questions a health
// check cannot: does it send anyone who visits it somewhere else, and can anyone
// actually use it.
//
// Runs after the health gate, on non-clone restores, and never rolls anything
// back. The container is healthy; what these produce is the finding, and a
// finding is worth more than a refusal here because the fix is a configuration
// value the operator owns.
func (e *Engine) verifyRestoredHTTP(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions) {
	if man == nil || opts.AsName != "" {
		return
	}
	ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
	insp, err := cli.ContainerInspect(ictx, opts.TargetID)
	cancel()
	if err != nil || insp.Config == nil {
		return
	}
	exposed := make([]string, 0, len(insp.Config.ExposedPorts))
	for p := range insp.Config.ExposedPorts {
		exposed = append(exposed, string(p))
	}
	sort.Strings(exposed)

	var spec *HTTPVerifySpec
	if profile := ProfileFor(manifestImage(man, b)); profile != nil {
		spec = profile.HTTPVerify
	}
	port := httpProbePort(spec, exposed)
	if port == 0 {
		return // nothing listening that this can ask
	}

	e.checkOffHostRedirect(ctx, cli, b, man, opts, port, insp.Name)
	if spec != nil && spec.LoginPath != "" {
		e.checkLoginRoundTrip(ctx, cli, b, man, opts, port, spec)
	}
}

// checkOffHostRedirect fetches "/" without following anything.
func (e *Engine) checkOffHostRedirect(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, port int, containerName string) {
	out, err := dockercli.CaptureSidecarInNetns(ctx, cli, opts.TargetID,
		[]string{"/bin/sh", "-c", rawRequestScript(port, "GET", "/", nil, "")})
	if err != nil {
		e.logf(b.ID, "INFO", "Could not ask the restored application what it answers on port %d (%v) — it passed its health check, but where it sends a visitor was not checked", port, err)
		return
	}
	resp, ok := parseHTTPResponse(string(out))
	if !ok {
		return // not HTTP on that port; nothing to conclude
	}
	offHost, host := redirectVerdict(resp.Status, resp.Location, ownAddressesFor(opts.NewSiteAddress, containerName))
	if !offHost {
		if resp.Status >= 300 && resp.Status <= 399 {
			e.logf(b.ID, "INFO", "The restored application redirects within itself — a visitor stays on this container")
			return
		}
		e.logf(b.ID, "INFO", "The restored application answers on port %d with %d and sends nobody elsewhere", port, resp.Status)
		return
	}

	e.addFinding(man, b.ID, findingOffHostRedirect, FindingDanger, describeRedirect(host, resp.Location), fmt.Sprintf(
		"Opening this restored container sends the visitor to %s, which is not this container. Anyone testing it — including whoever signs off on this restore — follows that redirect, sees a working application, and is looking at the ORIGINAL rather than the copy. "+
			"The application builds its redirects from a recorded address rather than from the host it is asked for; set that address to this host (the restore dialog's site-address field does it) and restore again, or change it in the application's own configuration.",
		resp.Location))
}

// checkLoginRoundTrip proves the application will accept a write from this host,
// not merely render a page.
//
// Deliberately-wrong credentials, per R4's method, under a recognisable username
// so the attempt reads as what it is in the application's own log. This is one
// failed login: on an application with a lockout policy that is not free, which
// is why it runs only where a profile declares the recipe.
func (e *Engine) checkLoginRoundTrip(ctx context.Context, cli *client.Client, b *store.Backup, man *Manifest, opts RestoreOptions, port int, spec *HTTPVerifySpec) {
	formOut, err := dockercli.CaptureSidecarInNetns(ctx, cli, opts.TargetID,
		[]string{"/bin/sh", "-c", rawRequestScript(port, "GET", spec.LoginPath, nil, "")})
	if err != nil {
		return
	}
	form, ok := parseHTTPResponse(string(formOut))
	if !ok || form.Status != 200 {
		return
	}
	token, found := extractFormToken(form.Body, spec.TokenPattern)
	if !found {
		// No token means no round trip worth making: POSTing without one earns
		// the 403 that means "no token", which is not the 403 being looked for.
		e.logf(b.ID, "INFO", "The restored application's login page did not carry the anti-forgery field this check knows about, so whether it accepts a login from this host was not tested")
		return
	}
	headers := []string{"Cookie: " + strings.Join(form.Cookies, "; ")}
	if origin := strings.TrimSpace(opts.NewSiteAddress); origin != "" {
		headers = append(headers, "Origin: "+origin, "Referer: "+origin+spec.LoginPath)
	}
	postOut, perr := dockercli.CaptureSidecarInNetns(ctx, cli, opts.TargetID,
		[]string{"/bin/sh", "-c", rawRequestScript(port, "POST", spec.LoginPath, headers, loginFormBody(spec.TokenField, token))})
	if perr != nil {
		return
	}
	post, pok := parseHTTPResponse(string(postOut))
	if !pok {
		return
	}
	if !csrfVerdict(post.Status) {
		e.logf(b.ID, "INFO", "The restored application accepted a login attempt from this host (HTTP %d) — its page renders AND it can be used, which a GET alone never showed", post.Status)
		return
	}
	e.addFinding(man, b.ID, findingOriginRejected, FindingDanger, "", fmt.Sprintf(
		"Origin rejected — the clone renders but cannot be used. This restored application serves its login page perfectly and refuses the login itself with HTTP 403, because it validates the Origin header against a list this host is not on. "+
			"Nobody can sign in, and it presents as \"the app works but my password is broken\". Add this host's address to the application's trusted-origins setting (adding to the list, never replacing it) and restart it."))
}
