// File overview: Keeps the browser's offer to install the page as an app, so the
// account menu can make it. This is how the mail app gets installed beside the
// calendar app on Android: Chrome's own menu entry (up to Chrome 157) refuses
// any second app on an origin that already has one, while the page's own
// prompt is decided by the manifest it links and installs it.

import { useSyncExternalStore } from "react";

/** BeforeInstallPromptEvent is the part of Chrome's event this file uses. */
type BeforeInstallPromptEvent = Event & {
  prompt: () => Promise<void>;
  userChoice: Promise<{ outcome: "accepted" | "dismissed" }>;
};

let offer: BeforeInstallPromptEvent | null = null;
const listeners = new Set<() => void>();

function changed(next: BeforeInstallPromptEvent | null) {
  if (offer === next) return;
  offer = next;
  for (const listener of listeners) listener();
}

/**
 * watchInstallPrompt starts listening before the app renders, because Chrome
 * fires the offer once per manifest as soon as it has checked the page, which
 * can be before React has mounted anything that could ask for it.
 */
export function watchInstallPrompt(target: Window = window): void {
  target.addEventListener("beforeinstallprompt", (event) => {
    // Held rather than shown: the reader asks for it from the account menu.
    event.preventDefault();
    changed(event as BeforeInstallPromptEvent);
  });
  target.addEventListener("appinstalled", () => changed(null));
}

/**
 * forgetInstallPrompt drops an offer made for a manifest the page no longer
 * links. Chrome checks the new manifest and makes a fresh offer for it, if it
 * has one to make; the old one would install the app the reader just left.
 */
export function forgetInstallPrompt(): void {
  changed(null);
}

/** installOffered reports whether the browser has an install to offer. */
export function installOffered(): boolean {
  return offer !== null;
}

/**
 * promptInstall shows the browser's install dialog. An offer is good for one
 * prompt, so it is spent whatever the reader answers - and whether or not the
 * browser still honours it: Chrome rejects the prompt of an offer it has since
 * withdrawn, which installs nothing, and that is all the caller needs to know.
 */
export async function promptInstall(): Promise<boolean> {
  const current = offer;
  if (!current) return false;
  changed(null);
  try {
    await current.prompt();
    const choice = await current.userChoice;
    return choice.outcome === "accepted";
  } catch {
    return false;
  }
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** useInstallOffered re-renders when the browser makes or withdraws an offer. */
export function useInstallOffered(): boolean {
  return useSyncExternalStore(subscribe, installOffered, () => false);
}
