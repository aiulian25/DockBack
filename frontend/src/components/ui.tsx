// Reusable UI primitives matching DESIGN.md components (buttons, status chips,
// cards, inputs, progress, modal). Soft 4px/8px radii, Docker-blue accents,
// tonal layering over heavy shadows.
import React, { createContext, useContext, useEffect, useId, useLayoutEffect, useRef } from "react";
import { createPortal } from "react-dom";

type Variant = "primary" | "secondary" | "ghost" | "danger";
type Size = "sm" | "md";

export function Button({
  variant = "secondary", size = "md", className = "", children, ...props
}: { variant?: Variant; size?: Size } & React.ButtonHTMLAttributes<HTMLButtonElement>) {
  const sizes: Record<Size, string> = {
    sm: "gap-1.5 px-2 py-1 text-xs",
    md: "gap-1.5 px-3 py-1.5 text-sm",
  };
  const base =
    `inline-flex items-center justify-center rounded font-medium transition-colors disabled:opacity-40 disabled:cursor-not-allowed ${sizes[size]}`;
  const styles: Record<Variant, string> = {
    primary: "bg-docker-blue text-white hover:bg-[#1f86d6]",
    secondary: "border border-outline-variant bg-surface-high/40 text-on-surface hover:bg-surface-high",
    ghost: "text-on-surface-variant hover:text-on-surface hover:bg-surface-high/50",
    danger: "bg-error-container text-error hover:brightness-110",
  };
  return (
    <button className={`${base} ${styles[variant]} ${className}`} {...props}>
      {children}
    </button>
  );
}

export function Chip({ kind, children }: { kind: "ok" | "warn" | "err" | "info" | "muted"; children: React.ReactNode }) {
  const map = {
    ok: "bg-success/10 text-success",
    warn: "bg-warning/10 text-warning",
    err: "bg-error/10 text-error",
    info: "bg-secondary/10 text-secondary",
    muted: "bg-on-surface-variant/10 text-on-surface-variant",
  } as const;
  return (
    <span className={`inline-flex items-center gap-1.5 rounded px-2 py-0.5 text-xs font-semibold uppercase tracking-wide ${map[kind]}`}>
      {children}
    </span>
  );
}

export function Card({ className = "", children, ...rest }: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div className={`rounded-lg border border-outline-variant/60 bg-surface-container ${className}`} {...rest}>{children}</div>
  );
}

export function Input(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      {...props}
      className={`w-full rounded border border-outline-variant bg-surface-lowest px-3 py-1.5 text-sm text-on-surface placeholder:text-on-surface-variant/60 outline-none focus:border-docker-blue focus:ring-2 focus:ring-docker-blue/40 ${props.className || ""}`}
    />
  );
}

// The controls a label can name.
const LABELLABLE = "input, select, textarea";

/**
 * Label names a form control for assistive technology. Pass htmlFor when the control
 * has an id. Without one, the label names the control that follows it, which is how
 * every form here lays a label out; a label not followed by a control stays plain
 * text, as before.
 */
export function Label({ children, htmlFor, ...rest }: React.LabelHTMLAttributes<HTMLLabelElement>) {
  const ref = useRef<HTMLLabelElement>(null);
  const generatedId = useId();
  useLayoutEffect(() => {
    const label = ref.current;
    if (!label || htmlFor) return;
    const next = label.nextElementSibling;
    const control = next?.matches(LABELLABLE) ? next : next?.querySelector(LABELLABLE);
    if (!control) return;
    if (!control.id) control.id = generatedId;
    label.htmlFor = control.id;
  });
  return (
    <label ref={ref} htmlFor={htmlFor} className="mb-1 block text-xs font-medium uppercase tracking-wider text-on-surface-variant" {...rest}>
      {children}
    </label>
  );
}

export function Select(props: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      {...props}
      className={`w-full rounded border border-outline-variant bg-surface-lowest px-3 py-1.5 text-sm text-on-surface outline-none focus:border-docker-blue focus:ring-2 focus:ring-docker-blue/40 ${props.className || ""}`}
    />
  );
}

export function Progress({ value }: { value: number }) {
  return (
    <div className="h-1 w-full overflow-hidden rounded-full bg-surface-highest">
      <div className="h-full rounded-full bg-gradient-to-r from-secondary to-docker-blue transition-all" style={{ width: `${Math.min(100, Math.max(0, value))}%` }} />
    </div>
  );
}

// What Tab can reach inside a dialog.
const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

function focusableIn(root: HTMLElement): HTMLElement[] {
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE));
}

// Open dialogs and how deeply each is nested. The deepest one is in front; every
// other child of the page is out of reach — by Tab, by a click and by a screen reader.
// Depth, not order of opening, decides, because a dialog and one nested inside it can
// open together and React sets up the inner one first.
const openDialogs: { layer: HTMLElement; depth: number }[] = [];
const madeInert = new Set<Element>();
const DialogDepth = createContext(0);

function frontDialog(): HTMLElement | undefined {
  return openDialogs.reduce<{ layer: HTMLElement; depth: number } | undefined>(
    (front, entry) => (!front || entry.depth >= front.depth ? entry : front), undefined)?.layer;
}

// Only what a dialog made inert is ever given back, so an element something else
// made inert stays that way.
function syncInert() {
  const front = frontDialog();
  for (const child of Array.from(document.body.children)) {
    const shouldBeInert = front !== undefined && child !== front;
    if (shouldBeInert && !child.hasAttribute("inert")) {
      child.setAttribute("inert", "");
      madeInert.add(child);
    } else if (!shouldBeInert && madeInert.has(child)) {
      child.removeAttribute("inert");
      madeInert.delete(child);
    }
  }
}

/**
 * Modal is the shared dialog: labelled by its title, with the rest of the page out of
 * reach while it is open. Focus goes in on open (to a field that asked for it, else
 * the first control), Tab and Shift-Tab stay inside, Escape closes it, and focus
 * returns to whatever opened it. dismissible=false stops Escape and the backdrop from
 * closing it, for a dialog with an irreversible operation in flight; its own buttons
 * still decide.
 */
export function Modal({ open, onClose, title, children, footer, wide, dismissible = true }: {
  open: boolean; onClose: () => void; title: string; children: React.ReactNode; footer?: React.ReactNode;
  // wide: for table-bearing dialogs (e.g. the stack-backup panel) — same shell,
  // larger max width.
  wide?: boolean;
  dismissible?: boolean;
}) {
  const titleId = useId();
  const depth = useContext(DialogDepth) + 1;
  const layer = useRef<HTMLDivElement>(null);
  const dialog = useRef<HTMLDivElement>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  const dismissibleRef = useRef(dismissible);
  dismissibleRef.current = dismissible;

  useEffect(() => {
    if (!open || !layer.current || !dialog.current) return;
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const entry = { layer: layer.current, depth };
    openDialogs.push(entry);
    syncInert();
    if (frontDialog() === entry.layer && !dialog.current.contains(document.activeElement)) {
      (focusableIn(dialog.current)[0] ?? dialog.current).focus();
    }
    return () => {
      openDialogs.splice(openDialogs.indexOf(entry), 1);
      syncInert();
      opener?.focus();
    };
  }, [open, depth]);

  if (!open) return null;

  const onKeyDown = (event: React.KeyboardEvent) => {
    if (event.key === "Escape") {
      event.stopPropagation();
      if (dismissibleRef.current) onCloseRef.current();
      return;
    }
    // A dialog opened from inside this one handles its own Tab.
    if (event.key !== "Tab" || !dialog.current || !dialog.current.contains(event.target as Node)) return;
    const focusable = focusableIn(dialog.current);
    if (focusable.length === 0) {
      event.preventDefault();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };

  return createPortal(
    <div ref={layer} className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-2 sm:p-4"
      onClick={() => { if (dismissible) onClose(); }} onKeyDown={onKeyDown}>
      <div ref={dialog} role="dialog" aria-modal="true" aria-labelledby={titleId} tabIndex={-1}
        className={`flex max-h-[95vh] w-full ${wide ? "max-w-3xl" : "max-w-lg"} flex-col rounded-lg border border-outline-variant bg-surface-high shadow-2xl outline-none sm:max-h-[90vh]`}
        onClick={(e) => e.stopPropagation()}>
        <h2 id={titleId} className="shrink-0 border-b border-outline-variant/60 px-4 py-3 text-base font-semibold">{title}</h2>
        <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3"><DialogDepth.Provider value={depth}>{children}</DialogDepth.Provider></div>
        {footer && <div className="shrink-0 flex justify-end gap-2 border-t border-outline-variant/60 px-4 py-3">{footer}</div>}
      </div>
    </div>,
    document.body,
  );
}

export function Spinner() {
  return <div className="h-4 w-4 animate-spin rounded-full border-2 border-on-surface-variant/30 border-t-docker-blue" />;
}
