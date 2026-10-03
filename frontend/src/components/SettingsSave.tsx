// Global "unsaved changes" coordination for the Settings page. Each savable
// section registers a {dirty, save} entry; a single sticky bar saves them all,
// and navigating away (in-app or tab close) is blocked while anything is dirty.
// Replaces the per-section Save buttons with one global Save.
import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { useBlocker } from "react-router-dom";
import { Loader2, AlertTriangle, CheckCircle2, Save, X } from "lucide-react";
import { Button } from "./ui";

type Saver = { dirty: boolean; save: () => Promise<void>; reset?: () => void };

interface Ctx {
  register: (id: string, saver: Saver) => void;
  unregister: (id: string) => void;
  anyDirty: boolean;
  saving: boolean;
  saveAll: () => Promise<boolean>;
  resetAll: () => void;
}

const SettingsSaveCtx = createContext<Ctx | null>(null);

export function useSettingsSave(): Ctx {
  const c = useContext(SettingsSaveCtx);
  if (!c) throw new Error("useSettingsSave must be used within SettingsSaveProvider");
  return c;
}

// useRegisterSaver hooks a section's dirty state + (stable) save handler into the
// global bar, plus an optional `reset` that reverts the section to its last-saved
// baseline (driving the bar's Discard button). Both `save` and `reset` MUST be
// stable (wrap in useCallback reading refs) so this only re-registers when `dirty`
// flips, not on every keystroke.
export function useRegisterSaver(id: string, dirty: boolean, save: () => Promise<void>, reset?: () => void) {
  const { register, unregister } = useSettingsSave();
  useEffect(() => {
    register(id, { dirty, save, reset });
  }, [id, dirty, save, reset, register]);
  useEffect(() => () => unregister(id), [id, unregister]);
}

export function SettingsSaveProvider({ children }: { children: React.ReactNode }) {
  const savers = useRef<Map<string, Saver>>(new Map());
  const [, force] = useState(0);
  const recompute = useCallback(() => force((n) => n + 1), []);

  const register = useCallback((id: string, saver: Saver) => {
    const prev = savers.current.get(id);
    savers.current.set(id, saver);
    if (!prev || prev.dirty !== saver.dirty) recompute(); // only re-render on dirty transitions
  }, [recompute]);
  const unregister = useCallback((id: string) => {
    if (savers.current.delete(id)) recompute();
  }, [recompute]);

  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState("");
  const [savedFlash, setSavedFlash] = useState(false);

  const anyDirty = Array.from(savers.current.values()).some((s) => s.dirty);

  // Discard: revert every dirty section to its last-saved baseline. Each reset is a
  // plain setState back to the section's baseline, so the dirty flags clear and the
  // bar disappears on the next render.
  const resetAll = useCallback(() => {
    for (const s of Array.from(savers.current.values())) {
      if (s.dirty) s.reset?.();
    }
    setErr(""); recompute();
  }, [recompute]);

  const saveAll = useCallback(async (): Promise<boolean> => {
    setSaving(true); setErr("");
    try {
      for (const s of Array.from(savers.current.values())) {
        if (s.dirty) await s.save();
      }
      setSavedFlash(true); setTimeout(() => setSavedFlash(false), 2000);
      return true;
    } catch (e) {
      setErr((e as Error).message || "save failed");
      return false;
    } finally {
      setSaving(false);
      recompute();
    }
  }, [recompute]);

  // Block in-app navigation while there are unsaved changes.
  const blocker = useBlocker(anyDirty);

  // Block tab close / refresh / hard navigation too.
  useEffect(() => {
    if (!anyDirty) return;
    const onBeforeUnload = (e: BeforeUnloadEvent) => { e.preventDefault(); e.returnValue = ""; };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, [anyDirty]);

  return (
    <SettingsSaveCtx.Provider value={{ register, unregister, anyDirty, saving, saveAll, resetAll }}>
      {children}

      {/* Sticky global save bar — appears only when something is unsaved. */}
      {(anyDirty || savedFlash) && (
        <div className="sticky bottom-0 z-30 -mx-1 mt-6 flex flex-wrap items-center gap-3 rounded-lg border border-outline-variant bg-surface-high/95 px-4 py-3 shadow-2xl backdrop-blur">
          {savedFlash && !anyDirty ? (
            <span className="flex items-center gap-2 text-sm font-medium text-success"><CheckCircle2 size={16} /> All changes saved</span>
          ) : (
            <span className="flex items-center gap-2 text-sm font-medium text-on-surface"><AlertTriangle size={16} className="text-warning" /> You have unsaved changes</span>
          )}
          {err && <span className="text-sm text-error">{err}</span>}
          <div className="ml-auto flex items-center gap-2">
            <Button variant="ghost" onClick={resetAll} disabled={saving || !anyDirty}>
              <X size={15} /> Discard
            </Button>
            <Button variant="primary" onClick={saveAll} disabled={saving || !anyDirty}>
              {saving ? <Loader2 size={15} className="animate-spin" /> : <Save size={15} />} Save changes
            </Button>
          </div>
        </div>
      )}

      {/* Leave-with-unsaved-changes confirmation (in-app navigation). */}
      {blocker.state === "blocked" && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
          <div className="w-full max-w-sm rounded-lg border border-outline-variant bg-surface-high p-6 shadow-2xl">
            <div className="mb-2 flex items-center gap-2 text-lg font-semibold text-on-surface">
              <AlertTriangle size={18} className="text-warning" /> Unsaved changes
            </div>
            <p className="text-sm text-on-surface-variant">You have unsaved settings. Leave this page and discard them?</p>
            <div className="mt-5 flex justify-end gap-2">
              <Button variant="ghost" onClick={() => blocker.reset?.()}>Stay</Button>
              <Button
                variant="primary"
                onClick={async () => { if (await saveAll()) blocker.proceed?.(); }}
                disabled={saving}
              >
                {saving ? <Loader2 size={15} className="animate-spin" /> : <Save size={15} />} Save &amp; leave
              </Button>
              <Button variant="danger" onClick={() => blocker.proceed?.()}>Discard &amp; leave</Button>
            </div>
          </div>
        </div>
      )}
    </SettingsSaveCtx.Provider>
  );
}
