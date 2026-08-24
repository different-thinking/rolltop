// File overview: React entrypoint. It mounts the root App component into the static shell served by Go.

import * as React from "react";
import { StrictMode } from "react";
import { createPortal } from "react-dom";
import { createRoot } from "react-dom/client";
import "@fontsource/fraunces/latin-700.css";
import App from "./App";
import { Icon, LogoMark } from "./components/Icon";
import { installRolltopPluginIconRuntime } from "./plugins/shared/iconRuntime";
import { installRolltopPluginReactRuntime } from "./plugins/shared/reactRuntime";
import "./styles.scss";

installRolltopPluginReactRuntime({ React, ReactDOM: { createPortal } });
// Before anything can load a plugin bundle: those import `components/Icon`
// through a shim that reads this, and the shim has nothing to fall back on.
installRolltopPluginIconRuntime({ Icon, LogoMark });

createRoot(document.getElementById("root") as HTMLElement).render(
  <StrictMode>
    <App />
  </StrictMode>
);

if ("serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    navigator.serviceWorker
      .register("/sw.js")
      .then((registration) => registration.update())
      .catch(() => {
        // The app still works without the offline cache.
      });
  });
}
