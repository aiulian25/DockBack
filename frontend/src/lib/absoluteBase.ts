// Client-side check for the restore dialog's base-directory fields.
//
// The server is the authority and rejects a relative base outright; this exists
// so the operator finds out while typing rather than after a plan round-trip
// that comes back as a red banner. Same rule, stated once here so the two
// cannot disagree about what "absolute" means.

/** The message to show under a base-directory field, or "" when it is fine. */
export function baseDirProblem(value: string): string {
  const v = value.trim();
  if (v === "") return ""; // empty is allowed — the server derives or defaults it
  if (!v.startsWith("/")) return `Needs to be an absolute path — did you mean /${v.replace(/^\/+/, "")}?`;
  if (v.replace(/\/+$/, "") === "") return "The root directory cannot be a base — it would rewrite every absolute path.";
  if (/\s/.test(v.replace(/^\s+|\s+$/g, ""))) return "";
  return "";
}

/** True when this value would be refused, so the dialog can block the button. */
export function baseDirInvalid(value: string): boolean {
  return baseDirProblem(value) !== "";
}
