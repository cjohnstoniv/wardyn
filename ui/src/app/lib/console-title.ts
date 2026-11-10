// The tab title has one writer. The console sets its own title here (the shell's "<page> - <console>"), and a
// brand, while set, renames it on the way out. So leaving a brand restores the console's title exactly, and
// a brand name that equals a page's title ("Runs -" gives "Runs - Wardyn") is never mistaken for one.
let own: string | null = null;
let rename: ((title: string) => string) | null = null;

function apply() {
  if (own === null) own = document.title; // index.html's, until the console sets one
  const title = rename ? rename(own) : own;
  if (document.title !== title) document.title = title;
}

/** The console's own title, unbranded. */
export function setConsoleTitle(title: string) {
  own = title;
  apply();
}

/** How a brand renames the console's title, or null when no brand is set. */
export function setTitleBrand(fn: ((title: string) => string) | null) {
  rename = fn;
  apply();
  if (!fn) own = null; // the title is the console's own again; the next brand reads it afresh
}
