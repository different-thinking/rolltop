// File overview: `components/Icon` import shim for runtime plugin bundles.
//
// Aliased in `vite.plugins.config.ts`, so a plugin — and any host module a
// plugin pulls in, such as `SettingsUI` — keeps importing `components/Icon`
// unchanged and gets the host's components instead of its own copy of Phosphor.
//
// The lookup happens per render rather than once at module scope: a plugin
// bundle is imported by URL at an arbitrary point in the app's life, and
// resolving eagerly would make load order the difference between an icon and a
// thrown error.

import type { IconWeight } from "@phosphor-icons/react";
import { createElement } from "react";
import { rolltopPluginIconRuntime } from "./iconRuntime";

/** Icon renders a semantic Rolltop icon name through the host's icon map. */
export function Icon(props: { name: string; weight?: IconWeight }) {
  return createElement(rolltopPluginIconRuntime().Icon, props);
}

/** LogoMark renders the host's brand mark. */
export function LogoMark(props: { className?: string }) {
  return createElement(rolltopPluginIconRuntime().LogoMark, props);
}
