// Runtime theming (PLAN §5). Themes are CSS-variable palettes defined in
// index.css and selected by setting `data-theme` on <html>. The choice is
// persisted per-browser in localStorage and applied before first paint (an
// inline script in index.html) so there's no flash of the wrong theme.

export type ThemeId = "midnight" | "classic" | "light";

export interface ThemeMeta {
  id: ThemeId;
  name: string;
  description: string;
  scheme: "dark" | "light";
  // Swatch colors for the picker preview (page bg, card, accent, text).
  swatch: { bg: string; card: string; accent: string; text: string };
}

export const THEMES: ThemeMeta[] = [
  {
    id: "midnight",
    name: "Midnight",
    description: "Deep blue-black with ring gauges — the default infrastructure look.",
    scheme: "dark",
    swatch: { bg: "#0a0e16", card: "#10151f", accent: "#4d8fd6", text: "#e6edf3" },
  },
  {
    id: "classic",
    name: "Classic",
    description: "The original high-contrast navy theme.",
    scheme: "dark",
    swatch: { bg: "#051424", card: "#122131", accent: "#9ccaff", text: "#d4e4fa" },
  },
  {
    id: "light",
    name: "Light",
    description: "Clean light theme for bright environments.",
    scheme: "light",
    swatch: { bg: "#eef1f5", card: "#ffffff", accent: "#1f6fd0", text: "#1b2430" },
  },
];

export const DEFAULT_THEME: ThemeId = "midnight";
const STORAGE_KEY = "dockback.theme";

export function isThemeId(v: string | null): v is ThemeId {
  return v === "midnight" || v === "classic" || v === "light";
}

export function getTheme(): ThemeId {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (isThemeId(v)) return v;
  } catch {
    /* localStorage unavailable */
  }
  return DEFAULT_THEME;
}

export function applyTheme(id: ThemeId): void {
  document.documentElement.setAttribute("data-theme", id);
}

export function setTheme(id: ThemeId): void {
  applyTheme(id);
  try {
    localStorage.setItem(STORAGE_KEY, id);
  } catch {
    /* ignore persistence failure */
  }
}
