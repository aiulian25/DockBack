# Moving an app to a different address

Some applications record the address they are served at **inside their own state** — a trusted-hosts list, a site-URL setting, links baked into page content. A faithful restore faithfully puts the old one back. The archive is intact, the container is healthy, and the app is unreachable, because it is answering at an address it does not believe in.

DockBack handles this, but only when you tell it to.

## Most moves need nothing here

If the app keeps the **same address** after the move — you re-pointed DNS, or your reverse proxy or tunnel now sends traffic to the new host — there is nothing to change. Leave the address field blank.

This is worth stating plainly because the opposite mistake is common: running an address rewrite "to be safe" after a move that didn't change the address. For some apps that irreversibly edits your content for no reason.

## When it does matter

The restore panel tells you which case you are in, because the consequences differ enormously:

| App | If the address is wrong | Severity |
|---|---|---|
| **Nextcloud** | *"You are accessing the server from an untrusted domain"* — refuses every request | **Unreachable** |
| **Homepage** | Every request returns `HTTP 400 Bad Request` | **Unreachable** |
| **BookStack** | Works fine; links inside pages point at the old address | Cosmetic |
| **Wiki.js** | Works fine; notification emails and login redirects use the old address | Cosmetic |

For the first two, an unnoticed stale address is a restore that reports success and leaves you with a site that will not load. That's why the panel is styled as a warning for those apps rather than a note.

## What DockBack will and won't do for you

Fill in **New address** in the restore panel and DockBack acts according to one rule: **can this be undone?**

**Applied automatically — reversible**

- **Environment variables** (BookStack's `APP_URL`, Homepage's `HOMEPAGE_ALLOWED_HOSTS`). Set as the container is recreated. Recreate again and you're back where you started.
- **The application's own CLI** (Nextcloud's `occ`). Setting a config value is undone by setting it again.

**Printed for you to run — irreversible**

- **Content rewrites** (BookStack's `bookstack:update-url`). This edits URLs across your pages in place and cannot be undone, so DockBack shows you the exact command with your current address already filled in, and runs nothing.
- **Settings with no command** (Wiki.js's site URL lives in its database with no CLI). You get the exact place to change it in the app's own admin area.

DockBack never edits an application's config file by hand. A regex through `config.php` is how a working restore becomes a broken one — so where the app provides a supported tool, that tool is what gets used, and where it doesn't, you do it.

## Every container: Remap domain

Beside **Remap machine IP** in both restore dialogs sits **Remap domain** — the address mechanism that works for *every* container, whether DockBack knows the application or not. Give it the current domain and the new one, and every environment value carrying the old name gets the new one: base URLs, CORS lists, trusted-host lists. The scheme, port and path around the name survive.

Why a from → to pair instead of one "new address" field? Because for an unknown application, no rule over variable *names* can tell the container's own address from an upstream's. `APP_URL` and `OLLAMA_BASE_URL` both end in URL; one is where this app lives and the other is a service on a different machine that must not change. The old domain *can* tell them apart: only values that actually carry it are rewritten, so an upstream pointing elsewhere is never touched.

Exact hosts only. A subdomain is a different host — `www.car.example.uk` does not change when you remap `car.example.uk` — and a name that merely contains yours (`oscar.example.uk`) is never matched. Every change is logged.

**If the domain lives in a `.env`, that file is remapped too.** A compose file written as `APP_URL=https://${DOMAIN}` has no domain in it to rewrite; the container gets the right value because it is inspected live, but the file beside it still holds the old one. DockBack captures the stack's `.env` with the compose file and puts both through the remap, so the next `docker compose up` agrees with the container instead of undoing it. When the remap runs and matches nothing at all, the log warns rather than reassures — on a machine that has just moved, no match usually means the value is hiding behind a variable, not that it was already correct.

For applications DockBack has a profile for, the **New address** field remains the better tool: it writes each setting in the exact form that application needs, including through its own CLI where that is the supported way. The two can be used together; both write the same new name.

## Every other app: DockBack tells you what it found

The list of variables DockBack can rewrite will always be shorter than the list of applications people run — and the failure does not care whether DockBack knows the app. Any container that records where it lives keeps recording the **previous machine** after a move. The restore is faithful, every service comes up, and the site answers by redirecting somewhere that has moved.

What makes that expensive is the silence. So after a restore **onto a different node**, DockBack reads the container's environment and names the variables that record an address:

```
This container was restored onto a different machine and its environment still
records where it used to live. Check these variables:
  APP_URL=https://app.old-machine.uk
  OLLAMA_BASE_URL=http://10.168.1.80:11434
The data is correct — anything wrong here is a line in the compose file, not a
problem with the backup.
```

It reports and never rewrites. Guessing that a variable means an address because its name looks like one is exactly how a good restore becomes a broken app.

Two things keep the list worth reading:

- **Nothing that a move cannot invalidate.** Bind addresses (`0.0.0.0`), loopback, and single-label service names (`redis`, `socket-proxy`) are dropped — Docker resolves those identically on the new host. A list where every entry is irrelevant is a list that gets skimmed past.
- **No secrets, ever.** Variables named for credentials are excluded, and only the scheme and host of a value are printed — never the path or query. A Slack webhook is a URL whose *path* is the credential, and it lives under a key called `WEBHOOK_URL`.

## What happens for Nextcloud specifically

Nextcloud is the sharpest case, because `trusted_domains` lives in `config.php` **inside a captured volume** — the restore writes the old machine's address straight back in.

Give it a new address and DockBack, through `occ`:

1. Reads the current `trusted_domains` list.
2. **Replaces the stale entry in place** where the old address is present. The list ends up exactly as long as it was — one host swapped for another, which is what a move is.
3. If there is no stale entry to replace, it **adds one** and says so in the log. That widens the trust list by exactly one named host, which is what you asked for, but it is a different operation and it is reported differently.
4. Sets `overwritehost`, `overwrite.cli.url` and `overwriteprotocol` to match, so generated links and cron-sent emails agree with the trust list.
5. Reads the list back and records what it actually became.

### The environment beats `occ`, so both are set

The official image reads `OVERWRITEHOST`, `OVERWRITECLIURL` and `OVERWRITEPROTOCOL` from the **container's environment** on every request, through its own `config/reverse-proxy.config.php` — which is loaded *after* `config.php`. Whatever those variables say wins, and `occ` cannot override them.

That is worth knowing because of how it fails: a Nextcloud moved to a new machine keeps the old `OVERWRITEHOST` in its compose file, redirects every visitor to a host that has moved, and `occ config:system:set overwritehost` appears to succeed and changes nothing.

So DockBack sets those variables **on the container as it is recreated**, alongside the `occ` settings, and writes the host, the URL and the protocol each in the form its own variable expects. `NEXTCLOUD_TRUSTED_DOMAINS` is updated too — space-separated, the way the entrypoint reads it — and existing entries are kept, so the instance keeps answering everywhere it already answers.

If the container is restored **without** being recreated, the environment cannot be changed that way. DockBack notices, and says plainly that the variable is overriding what `occ` just set and needs changing in your compose file.

`localhost` is never the entry it overwrites — `occ` and cron reach the instance that way, and taking it away would break them in a way that looks unrelated.

### `trusted_proxies` depends on whether the machine changed

It is not the same kind of value. `trusted_domains` is *what address is this server reached at*; `trusted_proxies` is *which upstream IP is allowed to set `X-Forwarded-For`*. A new site address on its own tells DockBack nothing about the second — so it is decided from whether the network path in front of the container actually changed.

**Restoring onto the same machine: kept.** The reverse proxy in front of it is almost certainly the same one at the same address, and clearing it would break `X-Forwarded-For` handling for a proxy that is still there.

**Restoring onto a different machine, with a new address: cleared.** The container moved and its address moved with it, so the recorded upstream is provably not the one in front of it here. An entry that trusts a machine no longer in the path is a header-spoofing hole: whatever now answers at that address can claim to be any client, and Nextcloud's brute-force protection, rate limits and audit log all believe it.

**Restoring onto a different machine with no new address: reported.** That restore changes no settings at all, so the stale trust is named rather than removed.

Whatever happens, the old value is printed first, as the command that puts it back:

```
docker exec -u www-data <container> php occ config:system:set trusted_proxies 0 --value <proxy-ip>
```

If your container sets `TRUSTED_PROXIES` in its environment, the official image writes that back into `config.php` on every start — so clearing it with `occ` is undone at the next restart. DockBack says so when it finds one.

### Your own `config.php` is copied aside first

Before the first setting is changed, DockBack copies `config/config.php` to `config/config.php.dockback-<timestamp>.bak` inside the container. Every value it replaces is also printed in the run log as it goes, so a single setting can be put back by hand — the copy is the same guarantee for everything else in the file. Nothing is ever deleted, and the name deliberately does not end in `.config.php`, which Nextcloud would load as live configuration.

## A scheme is added when you leave it out

Type `docs.example.com` and DockBack writes each setting in the form the application needs: a bare host where a host is wanted, and `https://docs.example.com` where a URL is. Type `http://docs.example.com:8000` and that scheme and port are kept exactly.

This matters more than it looks. A bare host is the natural thing to type and is correct for a trusted-domain list — but an application that parses its address as a URL treats a missing scheme as a fatal error, not an untidy one. Paperless appends `PAPERLESS_URL` to its CSRF trusted origins, and Django then refuses to start at all:

```
SystemCheckError: … the values in the CSRF_TRUSTED_ORIGINS setting must start
with a scheme (usually http:// or https://) but found docs.example.com.
```

The restore succeeds, the container never comes up, and the reason is three layers below where the address was typed. BookStack's `APP_URL`, Mealie's `BASE_URL` and Nextcloud's `overwrite.cli.url` are the same shape and the same hazard, so the rule applies to all of them.

## Wildcards are rejected

Every value here is a trust list or an address the app answers on. `*` would make the restore "succeed" by switching off the protection the setting exists to provide, so DockBack refuses it and asks for the specific address.

The same applies to anything that isn't a plausible hostname, IP or URL. These values are passed to the application as single arguments with no shell involved at any step, so they cannot be interpreted as commands — the validation is a second layer, not the only one.

## If something fails

An address change that doesn't apply **never fails or rolls back the restore**. Your data is restored and correct; a reachability setting is one command away. Whenever DockBack can't apply one, the run log prints the exact command that fixes it.

## Restoring as a copy

The address field is ignored for an **isolated copy**. A clone is a dry run under a throwaway name, so pointing it at your real address would be wrong — and, for a trust list, actively confusing.
