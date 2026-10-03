// Lightweight toast + undo layer (Fable-UI-UX A3). No dependency — a context
// provider mounted once at the app root exposes useToast() with success/error/
// info and an `undo` variant. The undo toast DEFERS the real action for a grace
// window (default 5s): onCommit runs only if the user doesn't press Undo, so no
// data is destroyed early and no backend change is needed.
import { createContext, useCallback, useContext, useEffect, useRef, useState } from "react";
import { CheckCircle2, AlertTriangle, Info, X, Undo2 } from "lucide-react";

type Kind = "success" | "error" | "info" | "undo" | "action";

interface ToastItem {
  id: number;
  kind: Kind;
  message: string;
  timeout: number;
  onUndo?: () => void;
  onCommit?: () => void | Promise<void>;
  actionLabel?: string;
  onAction?: () => void | Promise<void>;
}

export interface ToastApi {
  success: (message: string, ms?: number) => void;
  error: (message: string, ms?: number) => void;
  info: (message: string, ms?: number) => void;
  // undo defers onCommit until the window elapses; Undo cancels it (and runs onUndo).
  undo: (opts: { message: string; onCommit: () => void | Promise<void>; onUndo?: () => void; timeout?: number }) => void;
  // action shows a warning toast with one labeled button; onAction runs ONLY on
  // click (never on timeout/dismiss). Longer default window since it's actionable.
  action: (opts: { message: string; actionLabel: string; onAction: () => void | Promise<void>; timeout?: number }) => void;
}

const Ctx = createContext<ToastApi | null>(null);

export function useToast(): ToastApi {
  const c = useContext(Ctx);
  if (!c) throw new Error("useToast must be used within <ToastProvider>");
  return c;
}

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const timers = useRef<Map<number, ReturnType<typeof setTimeout>>>(new Map());
  const seq = useRef(0);

  const clearTimer = (id: number) => {
    const t = timers.current.get(id);
    if (t) { clearTimeout(t); timers.current.delete(id); }
  };

  const remove = useCallback((id: number) => {
    clearTimer(id);
    setItems((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const push = useCallback((t: Omit<ToastItem, "id">) => {
    const id = ++seq.current;
    setItems((prev) => [...prev, { ...t, id }]);
    timers.current.set(id, setTimeout(() => {
      // For an undo toast, letting the timer elapse COMMITS the action.
      if (t.kind === "undo") Promise.resolve(t.onCommit?.()).catch(() => {});
      remove(id);
    }, t.timeout));
    return id;
  }, [remove]);

  const api = useRef<ToastApi>({
    success: (m, ms = 4000) => push({ kind: "success", message: m, timeout: ms }),
    error: (m, ms = 6000) => push({ kind: "error", message: m, timeout: ms }),
    info: (m, ms = 4000) => push({ kind: "info", message: m, timeout: ms }),
    undo: ({ message, onCommit, onUndo, timeout = 5000 }) => push({ kind: "undo", message, onCommit, onUndo, timeout }),
    action: ({ message, actionLabel, onAction, timeout = 10000 }) => push({ kind: "action", message, actionLabel, onAction, timeout }),
  }).current;

  // Run an action toast's button, then close it. onAction never runs on timeout.
  const doAction = (t: ToastItem) => { Promise.resolve(t.onAction?.()).catch(() => {}); remove(t.id); };

  // Undo: cancel the pending commit and run onUndo. Dismiss (X) on an undo toast
  // COMMITS immediately (the action stays done); on other kinds it just closes.
  const doUndo = (t: ToastItem) => { clearTimer(t.id); t.onUndo?.(); setItems((p) => p.filter((x) => x.id !== t.id)); };
  const dismiss = (t: ToastItem) => {
    clearTimer(t.id);
    if (t.kind === "undo") Promise.resolve(t.onCommit?.()).catch(() => {});
    setItems((p) => p.filter((x) => x.id !== t.id));
  };

  useEffect(() => () => { timers.current.forEach((t) => clearTimeout(t)); timers.current.clear(); }, []);

  return (
    <Ctx.Provider value={api}>
      {children}
      <div className="pointer-events-none fixed bottom-4 right-4 z-[70] flex w-[min(92vw,22rem)] flex-col gap-2">
        {items.map((t) => (
          <ToastCard key={t.id} t={t} onUndo={() => doUndo(t)} onAction={() => doAction(t)} onDismiss={() => dismiss(t)} />
        ))}
      </div>
    </Ctx.Provider>
  );
}

function ToastCard({ t, onUndo, onAction, onDismiss }: { t: ToastItem; onUndo: () => void; onAction: () => void; onDismiss: () => void }) {
  const accent =
    t.kind === "success" ? "text-success" :
    t.kind === "error" ? "text-error" :
    t.kind === "action" ? "text-warning" :
    t.kind === "undo" ? "text-secondary" : "text-primary";
  const Icon = t.kind === "success" ? CheckCircle2 : t.kind === "error" || t.kind === "action" ? AlertTriangle : t.kind === "undo" ? Undo2 : Info;
  return (
    <div className="pointer-events-auto relative overflow-hidden rounded-lg border border-outline-variant bg-surface-high shadow-2xl">
      <div className="flex items-start gap-3 px-4 py-3">
        <Icon size={17} className={`mt-0.5 shrink-0 ${accent}`} />
        <span className="min-w-0 flex-1 text-sm text-on-surface">{t.message}</span>
        {t.kind === "undo" && (
          <button onClick={onUndo} className="shrink-0 rounded px-2 py-0.5 text-xs font-semibold text-primary hover:bg-docker-blue/10">Undo</button>
        )}
        {t.kind === "action" && t.actionLabel && (
          <button onClick={onAction} className="shrink-0 self-center rounded border border-warning/50 bg-warning/10 px-2.5 py-1 text-xs font-semibold text-warning hover:bg-warning/20">{t.actionLabel}</button>
        )}
        <button onClick={onDismiss} aria-label="Dismiss" className="shrink-0 rounded p-0.5 text-on-surface-variant hover:bg-surface-highest hover:text-on-surface"><X size={14} /></button>
      </div>
      {t.kind === "undo" && (
        // Countdown bar: shrinks over the grace window so the deadline is visible.
        <div className="h-0.5 w-full bg-outline-variant/40">
          <div className="h-full bg-secondary" style={{ animation: `toast-countdown ${t.timeout}ms linear forwards` }} />
        </div>
      )}
    </div>
  );
}
