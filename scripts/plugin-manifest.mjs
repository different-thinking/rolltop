// File overview: the single derivation of "which runtime files a plugin
// manifest declares", shared by build-plugins.mjs (which checks the build
// actually produced them) and assemble-plugin-dist.mjs (which copies their
// directories into the deployable tree). The server serves exactly these paths
// (pluginAssetAllowed in backend/web/plugin_routes.go) and stats the theme CSS
// at startup, so the two scripts have to agree on the list to the letter —
// which is why it lives in one place rather than in each of them.

import path from "node:path";

// normalizeManifestRelative turns a manifest-declared path into a clean,
// repo-relative path under the plugin's own directory: leading slashes stripped
// and "." / ".." resolved, so a caller can join it against the plugin root.
export function normalizeManifestRelative(relative) {
  return path.posix.normalize(relative.replace(/^\/+/, ""));
}

// declaredManifestFiles returns the runtime files a manifest names — the
// frontend module and CSS bundles and every theme CSS — as normalized
// repo-relative paths. `backend.binary` is deliberately absent: the .so files
// come from the Go build stage, not from the frontend tree these scripts walk.
export function declaredManifestFiles(manifest) {
  return [
    manifest.frontend?.module,
    manifest.frontend?.css,
    ...(manifest.themes ?? []).map((theme) => theme?.css)
  ]
    .filter(Boolean)
    .map(normalizeManifestRelative);
}
