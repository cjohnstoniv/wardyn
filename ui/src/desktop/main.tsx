/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Entry of desktop.html, the viewer the UI gateway serves for a Linux VM's desktop.
import React from "react";
import { createRoot } from "react-dom/client";
import "../styles/index.css";
import { DesktopViewer, readBoot } from "./viewer";

const boot = readBoot();
const root = document.getElementById("desktop-root");
if (root && boot) {
  createRoot(root).render(
    <React.StrictMode>
      <DesktopViewer boot={boot} />
    </React.StrictMode>,
  );
}
