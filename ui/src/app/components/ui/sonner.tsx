"use client";

import { useTheme } from "../wardyn/theme-provider";
import { Toaster as Sonner, ToasterProps } from "sonner";

const Toaster = ({ ...props }: ToasterProps) => {
  const { theme } = useTheme();

  return (
    <Sonner
      theme={theme as ToasterProps["theme"]}
      className="toaster group"
      style={
        {
          "--normal-bg": "var(--popover)",
          "--normal-text": "var(--popover-foreground)",
          "--normal-border": "var(--border)",
        } as React.CSSProperties
      }
      // F7-F13: 39 toast.error() call sites across the console, none passing
      // an explicit duration — sonner's stock 4s default auto-dismisses an
      // error before a longer message (getErrorMessage's server-message
      // pass-through) can be read. closeButton adds a manual-dismiss
      // affordance instead of forcing every reader to race the clock. Both
      // are DEFAULTS, before {...props} below, so a caller can still opt out
      // (<Toaster closeButton={false} />) or a per-call toast.error(msg,
      // {duration}) still wins for that one toast.
      closeButton
      toastOptions={{ duration: 8000 }}
      {...props}
    />
  );
};

export { Toaster };
