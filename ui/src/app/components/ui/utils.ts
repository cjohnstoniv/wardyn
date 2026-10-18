import { clsx, type ClassValue } from "clsx";
import { extendTailwindMerge } from "tailwind-merge";

// The theme's own font-size rungs (theme.css --text-meta/--text-body/--text-display).
// Unregistered, tailwind-merge reads them as text colours and drops the real colour
// class (or the size) when both are passed.
const twMerge = extendTailwindMerge({
  extend: { theme: { text: ["meta", "body", "display"] } },
});

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
