/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import React from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./app/App";
import { BrandingProvider } from "./app/components/wardyn/branding-context";
import { basePath } from "./app/lib/base-path";
import monoWoff2 from "@fontsource/jetbrains-mono/files/jetbrains-mono-latin-400-normal.woff2?url";
import "./styles/index.css";

// Vite hashes the font file, so a static index.html tag cannot name it.
const preload = document.createElement("link");
preload.rel = "preload";
preload.as = "font";
preload.type = "font/woff2";
preload.crossOrigin = "anonymous";
preload.href = monoWoff2;
document.head.appendChild(preload);

const container = document.getElementById("root");
if (!container) {
  throw new Error('Root element "#root" not found');
}

createRoot(container).render(
  <React.StrictMode>
    <BrowserRouter basename={basePath() || undefined}>
      <BrandingProvider>
        <App />
      </BrandingProvider>
    </BrowserRouter>
  </React.StrictMode>,
);
