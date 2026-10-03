// Clipboard helper that also works over plain-HTTP LAN access, where
// navigator.clipboard is unavailable (it requires a secure context). Falls back
// to a hidden textarea + execCommand("copy"). Returns whether the copy landed,
// so callers can confirm with a toast instead of failing silently.
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch { /* permission denied or insecure context — try the fallback */ }
  }
  try {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    // Off-screen but focusable; fixed so selecting it doesn't scroll the page.
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(ta);
    return ok;
  } catch {
    return false;
  }
}

// True while the user has text selected — row click handlers use this to NOT
// navigate on the mouse-up that ends a drag-selection, so values in clickable
// rows (endpoints, image references, IDs) can be copied by hand.
export function hasTextSelection(): boolean {
  return !!window.getSelection()?.toString();
}
