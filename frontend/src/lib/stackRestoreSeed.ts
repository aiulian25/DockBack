// Handing the caller's already-made choices to the stack restore PAGE (F224).
//
// The dialog this replaces took them as a prop. A page cannot, so they travel
// out-of-band — and deliberately NOT in the URL: these values end up in browser
// history, in any proxy's access log, and in the Referer header of the next
// request. A restore target and a path remap are not secrets, but a URL is the
// wrong place for them all the same, and the one value that IS a secret must
// never be near it.
//
// sessionStorage, one shot: written immediately before the navigation, read once
// on the page's first render, deleted on read. Per-tab, never sent to the
// server, and gone when the tab closes.

const SEED_KEY = "dback.stack_restore_seed";

// StackRestoreSeed is what a caller can carry over.
//
// There is no privateKey field, on purpose. The Backups drawer may already hold
// the offline key for a write-only backup, and the dialog used to pass it so the
// operator did not paste it twice — but reproducing that here means writing key
// material into a storage API to save one paste. The restore page asks for it
// instead, on the page that actually uses it.
export interface StackRestoreSeed {
  // Which stack this seed is for. Checked on read, so a stale seed can never be
  // applied to a different stack's restore.
  nodeID: string;
  project: string;

  // The clicked backup's app-consistent snapshot group: preselected as the point
  // in time when it is a COMPLETE group, so "Restore stack" from a specific
  // backup honors that backup's moment, not just "latest".
  group?: string;
  targetNode?: string;
  recreate?: boolean;
  snapshot?: boolean;
  reconstructHost?: boolean;
  hostBaseDir?: string;
  remapIP?: boolean;
  remapFrom?: string;
  remapTo?: string;
  remapPath?: boolean;
  pathFrom?: string;
  pathTo?: string;
  // F214: the copy the drawer was already reading from.
  source?: string;
  // F215: the choices the drawer already collected.
  remapDomain?: boolean;
  domainFrom?: string;
  domainTo?: string;
  newSiteAddress?: string;
  newUpstreamAddress?: string;
}

// writeStackRestoreSeed stores the seed for the next navigation. Best-effort:
// private mode or a full quota costs the operator a re-tick, never the restore.
export function writeStackRestoreSeed(seed: StackRestoreSeed) {
  try { sessionStorage.setItem(SEED_KEY, JSON.stringify(seed)); } catch { /* ignore */ }
}

// readStackRestoreSeed consumes the seed, if one was left for THIS stack.
//
// Always removes what it found, whether or not it matched: a seed that is not
// for this page is stale by definition, and leaving it would apply somebody's
// old selections to a later restore of a different stack.
export function readStackRestoreSeed(nodeID: string, project: string): StackRestoreSeed | null {
  let raw: string | null = null;
  try {
    raw = sessionStorage.getItem(SEED_KEY);
    if (raw !== null) sessionStorage.removeItem(SEED_KEY);
  } catch { return null; }
  if (!raw) return null;
  try {
    const s = JSON.parse(raw) as StackRestoreSeed;
    if (!s || s.nodeID !== nodeID || s.project !== project) return null;
    return s;
  } catch { return null; }
}
