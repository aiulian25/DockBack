// Pre-paint theme apply (perf Fix 10). Runs as a tiny render-blocking script in
// <head> — BEFORE first paint — so a saved non-default theme (e.g. light) never
// flashes the midnight default while the app bundle loads. External same-origin
// file: the strict CSP (default-src 'self', no inline scripts) allows it
// unchanged. Keep the storage key + ids in sync with src/theme.ts.
try {
  var t = localStorage.getItem("dockback.theme");
  if (t === "midnight" || t === "classic" || t === "light") {
    document.documentElement.setAttribute("data-theme", t);
  }
} catch (e) { /* localStorage unavailable — keep the default */ }
