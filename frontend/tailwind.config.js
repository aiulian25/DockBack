/** Tailwind config. Colors are CSS-variable-backed (alpha-aware) so the whole
 *  UI can be re-themed at runtime — Midnight (the dashboard redesign), Classic
 *  (the original "Technical Infrastructure Core"), and Light. Each token reads
 *  `--c-<name>` (space-separated RGB channels) defined per [data-theme] in
 *  index.css, so `bg-surface`, `text-primary`, `bg-primary/10`, etc. all theme. */
const c = (name) => `rgb(var(--c-${name}) / <alpha-value>)`;

/** @type {import('tailwindcss').Config} */
export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        surface: c("surface"),
        "surface-dim": c("surface-dim"),
        "surface-bright": c("surface-bright"),
        "surface-lowest": c("surface-lowest"),
        "surface-low": c("surface-low"),
        "surface-container": c("surface-container"),
        "surface-high": c("surface-high"),
        "surface-highest": c("surface-highest"),
        "on-surface": c("on-surface"),
        "on-surface-variant": c("on-surface-variant"),
        outline: c("outline"),
        "outline-variant": c("outline-variant"),
        primary: c("primary"),
        "primary-container": c("primary-container"),
        "on-primary": c("on-primary"),
        secondary: c("secondary"),
        "secondary-container": c("secondary-container"),
        tertiary: c("tertiary"),
        error: c("error"),
        "error-container": c("error-container"),
        success: c("success"),
        warning: c("warning"),
        "docker-blue": c("docker-blue"),
      },
      fontFamily: {
        sans: ["'Inter Variable'", "Inter", "system-ui", "sans-serif"],
        mono: ["'Geist Mono'", "ui-monospace", "monospace"],
      },
      borderRadius: {
        DEFAULT: "0.25rem",
        md: "0.375rem",
        lg: "0.5rem",
        xl: "0.75rem",
      },
      // Content width cap. Generous so 1080p/1440p monitors use the full screen
      // (no wasted left/right gutters); only bounds extreme ultrawide/4K so card
      // grids and prose don't stretch absurdly.
      maxWidth: { container: "2400px" },
    },
  },
  plugins: [],
};
