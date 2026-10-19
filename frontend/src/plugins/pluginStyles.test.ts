// @vitest-environment-options { "settings": { "disableCSSFileLoading": true, "handleDisabledFileLoadingAsSuccess": true } }
// The stylesheets these tests add are never served: happy-dom reports each one
// loaded, and the tests fire the failures themselves.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { pluginStylesheetRetryDelay, syncPluginStylesheets } from "./pluginStyles";

const spam = { id: "experimental_spam_filter", css_url: "/plugins/experimental_spam_filter/assets/frontend_dist/styles.css?v=1" };

function link(id = spam.id): HTMLLinkElement | null {
  return document.getElementById(`runtime-plugin-css-${id}`) as HTMLLinkElement | null;
}

describe("syncPluginStylesheets", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    document.head.innerHTML = "";
  });

  afterEach(() => {
    syncPluginStylesheets([]);
    vi.useRealTimers();
  });

  it("adds one stylesheet per plugin and leaves one in flight or applied alone", () => {
    syncPluginStylesheets([spam]);
    const first = link();
    expect(first?.href).toBe(new URL(spam.css_url, window.location.href).href);
    syncPluginStylesheets([spam]);
    expect(link()).toBe(first);
  });

  it("asks again for a stylesheet that failed, on a fresh element", () => {
    syncPluginStylesheets([spam]);
    const failed = link()!;
    failed.dispatchEvent(new Event("error"));
    expect(failed.dataset.loadState).toBe("failed");

    vi.advanceTimersByTime(pluginStylesheetRetryDelay(1));
    const retried = link()!;
    expect(retried).not.toBe(failed);
    expect(failed.isConnected).toBe(false);
    expect(new URL(retried.href).searchParams.get("retry")).toBe("1");

    retried.dispatchEvent(new Event("load"));
    expect(retried.dataset.loadState).toBe("loaded");
    vi.advanceTimersByTime(pluginStylesheetRetryDelay(5));
    expect(link()).toBe(retried);
  });

  it("asks again at once when the connection comes back", () => {
    syncPluginStylesheets([spam]);
    const failed = link()!;
    failed.dispatchEvent(new Event("error"));
    window.dispatchEvent(new Event("online"));
    expect(link()).not.toBe(failed);
  });

  it("asks again at once when the plugins are reloaded", () => {
    syncPluginStylesheets([spam]);
    const failed = link()!;
    failed.dispatchEvent(new Event("error"));
    syncPluginStylesheets([spam]);
    expect(link()).not.toBe(failed);
    expect(link()?.dataset.loadState).toBe("loading");
  });

  it("points a loaded stylesheet at a new build in place", () => {
    syncPluginStylesheets([spam]);
    const loaded = link()!;
    loaded.dispatchEvent(new Event("load"));
    const next = { ...spam, css_url: spam.css_url.replace("v=1", "v=2") };
    syncPluginStylesheets([next]);
    expect(link()).toBe(loaded);
    expect(loaded.href).toBe(new URL(next.css_url, window.location.href).href);

    loaded.dispatchEvent(new Event("error"));
    vi.advanceTimersByTime(pluginStylesheetRetryDelay(1));
    expect(new URL(link()!.href).searchParams.get("v")).toBe("2");
  });

  it("stops asking for a plugin that went away", () => {
    syncPluginStylesheets([spam]);
    link()!.dispatchEvent(new Event("error"));
    syncPluginStylesheets([]);
    expect(link()).toBeNull();
    vi.advanceTimersByTime(pluginStylesheetRetryDelay(10));
    window.dispatchEvent(new Event("online"));
    expect(link()).toBeNull();
  });
});

describe("pluginStylesheetRetryDelay", () => {
  it("backs off and is capped", () => {
    expect(pluginStylesheetRetryDelay(1)).toBe(2_000);
    expect(pluginStylesheetRetryDelay(2)).toBe(6_000);
    expect(pluginStylesheetRetryDelay(20)).toBe(5 * 60_000);
  });
});
