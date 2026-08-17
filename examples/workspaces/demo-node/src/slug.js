// Tiny string helpers — the agent will add slugify() here.

/** Upper-case the first letter of every whitespace-separated word. */
export function titleCase(s) {
  return String(s)
    .split(/\s+/)
    .filter(Boolean)
    .map((w) => w[0].toUpperCase() + w.slice(1).toLowerCase())
    .join(" ");
}

/** Collapse runs of whitespace down to a single space and trim. */
export function squish(s) {
  return String(s).replace(/\s+/g, " ").trim();
}
