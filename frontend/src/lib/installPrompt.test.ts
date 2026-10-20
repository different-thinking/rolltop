import { afterEach, describe, expect, it, vi } from "vitest";

import { syncDocumentManifest } from "./appIcon";
import { forgetInstallPrompt, installOffered, promptInstall, watchInstallPrompt } from "./installPrompt";

function installEvent(outcome: "accepted" | "dismissed" = "accepted") {
  const event = new Event("beforeinstallprompt", { cancelable: true }) as Event & {
    prompt: () => Promise<void>;
    userChoice: Promise<{ outcome: "accepted" | "dismissed" }>;
  };
  event.prompt = vi.fn(async () => {});
  event.userChoice = Promise.resolve({ outcome });
  return event;
}

const target = new EventTarget() as unknown as Window;
watchInstallPrompt(target);

afterEach(() => {
  forgetInstallPrompt();
  document.head.innerHTML = "";
});

describe("install prompt", () => {
  it("holds the browser's offer instead of letting it show on its own", () => {
    const event = installEvent();
    target.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    expect(installOffered()).toBe(true);
  });

  it("spends the offer on one prompt whatever the answer", async () => {
    const event = installEvent("dismissed");
    target.dispatchEvent(event);
    expect(await promptInstall()).toBe(false);
    expect(event.prompt).toHaveBeenCalledOnce();
    expect(installOffered()).toBe(false);
    expect(await promptInstall()).toBe(false);
  });

  it("answers a prompt the browser refuses as not installed", async () => {
    const event = installEvent();
    event.prompt = vi.fn(async () => {
      throw new DOMException("The prompt() method may only be called once.", "InvalidStateError");
    });
    target.dispatchEvent(event);
    expect(await promptInstall()).toBe(false);
    expect(installOffered()).toBe(false);
  });

  it("withdraws the offer once the app is installed", () => {
    target.dispatchEvent(installEvent());
    target.dispatchEvent(new Event("appinstalled"));
    expect(installOffered()).toBe(false);
  });

  it("drops an offer made for the manifest the page no longer links", () => {
    document.head.innerHTML = '<link rel="manifest" href="/calendar.webmanifest">';
    target.dispatchEvent(installEvent());
    syncDocumentManifest("/calendar/2026-10-19");
    expect(installOffered()).toBe(true);
    syncDocumentManifest("/mail/relevant");
    expect(installOffered()).toBe(false);
  });
});
