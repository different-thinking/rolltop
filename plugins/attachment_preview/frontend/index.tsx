// File overview: Attachment preview slot registry for optional preview plugins.

import { lazy, Suspense } from "react";
import type { Attachment } from "../../../frontend/src/types";
import { pluginEnabled, pluginIDs, type PluginSet } from "../../../frontend/src/plugins/registry";

// Nothing here may reference `./AttachmentPreviewAction` statically. It reaches
// PDFium, and a static import — or a re-export of the same binding — puts the
// viewer back in the entry chunk and reduces the `lazy()` below to ceremony,
// which is exactly what the earlier `export { AttachmentPreviewAction,
// PdfAttachmentViewer }` did. Vite says so out loud when it happens:
// "dynamically imported by index.tsx but also statically imported".
const LazyAttachmentPreviewAction = lazy(() =>
  import("./AttachmentPreviewAction").then((module) => ({ default: module.AttachmentPreviewAction }))
);

/** AttachmentPreviewSlot renders attachment preview UI only when the plugin and preview metadata are present. */
export function AttachmentPreviewSlot({ attachment, plugins }: { attachment: Attachment; plugins: PluginSet }) {
  if (!pluginEnabled(plugins, pluginIDs.attachmentPreview) || !attachment.preview?.available) return null;
  return (
    <Suspense fallback={null}>
      <LazyAttachmentPreviewAction attachment={attachment} />
    </Suspense>
  );
}

