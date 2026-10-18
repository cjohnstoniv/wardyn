import { clsx, type ClassValue } from "clsx";
import { extendTailwindMerge } from "tailwind-merge";

// The theme's own font-size rungs (theme.css --text-meta/--text-body/--text-display). Unregistered,
// tailwind-merge reads them as text colours and drops the real colour class (or the size). They get a
// group of their own, not the built-in `font-size` one: a built-in size carries a line-height, so
// tailwind-merge makes it drop `leading-*`, but these rungs set a size only, and dropping the leading
// would change every Chip and Label that passes one. A token and a built-in size still replace each other.
const twMerge = extendTailwindMerge<"font-size-token">({
  extend: {
    classGroups: { "font-size-token": [{ text: ["meta", "body", "display"] }] },
    conflictingClassGroups: { "font-size": ["font-size-token"], "font-size-token": ["font-size"] },
  },
});

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
