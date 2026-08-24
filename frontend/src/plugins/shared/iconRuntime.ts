// File overview: Shared icon access for browser-loaded frontend plugins.
//
// The twin of `reactRuntime.ts`, and it exists for a sharper reason. Plugins
// render icons through `components/Icon`, which imports its glyphs from the
// `@phosphor-icons/react` barrel — 4,543 modules. Tree shaking keeps the output
// honest, but every plugin bundle still had to *transform* all of them, and the
// icons it kept were a second copy of what the host already ships. Measured on
// one plugin: 4,548 modules and 222 kB became 8 modules and 12 kB.
//
// So the host installs its icon components here and plugin builds alias
// `components/Icon` to `iconShim.ts`. Phosphor then appears exactly once, in
// the application bundle.

import type { IconWeight } from "@phosphor-icons/react";
import type { ReactElement } from "react";

export type RolltopPluginIconRuntime = {
  Icon: (props: { name: string; weight?: IconWeight }) => ReactElement;
  LogoMark: (props: { className?: string }) => ReactElement;
};

type RuntimeGlobal = typeof globalThis & {
  __rolltopPluginIconRuntime?: RolltopPluginIconRuntime;
};

export function installRolltopPluginIconRuntime(runtime: RolltopPluginIconRuntime) {
  (globalThis as RuntimeGlobal).__rolltopPluginIconRuntime = runtime;
}

export function rolltopPluginIconRuntime(): RolltopPluginIconRuntime {
  const runtime = (globalThis as RuntimeGlobal).__rolltopPluginIconRuntime;
  if (!runtime) {
    throw new Error("Rolltop plugin icon runtime is not available.");
  }
  return runtime;
}
