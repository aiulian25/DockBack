// Reusable UI primitives matching DESIGN.md components (buttons, status chips,
// cards, inputs, progress, modal). Soft 4px/8px radii, Docker-blue accents,
// tonal layering over heavy shadows.
import React from "react";

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

export function Label({ children }: { children: React.ReactNode }) {
  return <label className="mb-1 block text-xs font-medium uppercase tracking-wider text-on-surface-variant">{children}</label>;
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

export function Modal({ open, onClose, title, children, footer, wide }: {
  open: boolean; onClose: () => void; title: string; children: React.ReactNode; footer?: React.ReactNode;
  // wide: for table-bearing dialogs (e.g. the stack-backup panel) — same shell,
  // larger max width.
  wide?: boolean;
}) {
  if (!open) return null;
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-2 sm:p-4" onClick={onClose}>
      <div className={`flex max-h-[95vh] w-full ${wide ? "max-w-3xl" : "max-w-lg"} flex-col rounded-lg border border-outline-variant bg-surface-high shadow-2xl sm:max-h-[90vh]`} onClick={(e) => e.stopPropagation()}>
        <div className="shrink-0 border-b border-outline-variant/60 px-4 py-3 text-base font-semibold">{title}</div>
        <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">{children}</div>
        {footer && <div className="shrink-0 flex justify-end gap-2 border-t border-outline-variant/60 px-4 py-3">{footer}</div>}
      </div>
    </div>
  );
}

export function Spinner() {
  return <div className="h-4 w-4 animate-spin rounded-full border-2 border-on-surface-variant/30 border-t-docker-blue" />;
}
