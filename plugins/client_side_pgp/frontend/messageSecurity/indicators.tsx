// Through the host's `Icon` rather than the `@phosphor-icons/react` barrel:
// the barrel is 4,543 modules, and importing it from a plugin pulls all of them
// into that plugin's build to keep two glyphs. `Icon` resolves `lock` and
// `signature` to these same components and applies the same class, aria-hidden
// and focusable attributes, so the rendered markup is unchanged.
import { Icon } from "../../../../frontend/src/components/Icon";
import type { MessageSecurityIndicatorContext, MessageSecurityState } from "../types";
import { pgpPreviewText } from "./preview";

export function pgpMessageSecurityPreviewText(snippet: string, state: MessageSecurityState) {
  return pgpPreviewText(snippet, state.is_encrypted, state.is_signed);
}

export function pgpMessageSecuritySnippetClassName(state: MessageSecurityState) {
  return state.is_encrypted ? "encrypted-preview" : "";
}

export function renderPGPMessageSecurityIndicators({ state }: MessageSecurityIndicatorContext) {
  if (!state.is_encrypted && !state.is_signed) return null;
  const label = [state.is_encrypted ? "Encrypted" : "", state.is_signed ? "Signed" : ""].filter(Boolean).join(", ");
  return (
    <span className="message-pgp-icons" aria-label={label}>
      {state.is_encrypted ? (
        <span className="message-pgp-icon encrypted" title="Encrypted message">
          <Icon name="lock" weight="bold" />
        </span>
      ) : null}
      {state.is_signed ? (
        <span className="message-pgp-icon signature pending" title="Signature pending verification">
          <Icon name="signature" weight="bold" />
        </span>
      ) : null}
    </span>
  );
}
