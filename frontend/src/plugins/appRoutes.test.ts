import { describe, expect, it } from "vitest";

import { appRouteErrorMessage, appRoutes, matchAppRoute, type RuntimePlugin, type RuntimePlugins } from "./runtime";

const page = () => null;

describe("appRoutes", () => {
  it("collects every plugin's pages in plugin order", () => {
    const plugins: RuntimePlugin[] = [
      { appRoutes: [{ path: "/files", nested: true, label: "Files", icon: "folder", render: page }] },
      { appRoutes: [{ path: "/notes", render: page }] },
      { accountSettingsRoutes: [] }
    ];
    expect(appRoutes(plugins).map((route) => route.path)).toEqual(["/files", "/notes"]);
  });

  it("normalizes the path and fills in the sidebar defaults", () => {
    const [route] = appRoutes([{ appRoutes: [{ path: "files/", render: page }] }]);
    expect(route.path).toBe("/files");
    expect(route.nested).toBe(false);
    expect(route.icon).toBe("folder");
    expect(route.label).toBe("");
  });

  it("drops a route that cannot draw anything or claims the root", () => {
    const plugins: RuntimePlugin[] = [{
      appRoutes: [
        { path: "/no-render" },
        { path: "/", render: page },
        { path: "   ", render: page },
        { path: "/good", render: page }
      ]
    }];
    expect(appRoutes(plugins).map((route) => route.path)).toEqual(["/good"]);
  });

  it("ignores a plugin whose appRoutes is not a list", () => {
    expect(appRoutes([{ appRoutes: "/files" }, { appRoutes: null }])).toEqual([]);
  });
});

describe("matchAppRoute", () => {
  const plugins: RuntimePlugin[] = [{
    appRoutes: [
      { path: "/files", nested: true, render: page },
      { path: "/flat", render: page }
    ]
  }];

  it("finds the page serving a path, and its descendants when nested", () => {
    expect(matchAppRoute(plugins, "/files")?.path).toBe("/files");
    expect(matchAppRoute(plugins, "/files/2026/05")?.path).toBe("/files");
    expect(matchAppRoute(plugins, "/flat")?.path).toBe("/flat");
  });

  it("returns nothing for a path no plugin declared", () => {
    expect(matchAppRoute(plugins, "/flat/child")).toBeNull();
    expect(matchAppRoute(plugins, "/filesx")).toBeNull();
    expect(matchAppRoute(plugins, "/mail")).toBeNull();
    expect(matchAppRoute([], "/files")).toBeNull();
  });

  it("ignores a query string or hash on the path it is asked about", () => {
    expect(matchAppRoute(plugins, "/files?open=1")?.path).toBe("/files");
  });
});

describe("appRouteErrorMessage", () => {
  const plugins = (errors: RuntimePlugins["errors"]): RuntimePlugins => ({ all: [], byID: {}, status: "ready", errors });

  it("names the plugin that failed and what it said", () => {
    expect(appRouteErrorMessage(plugins([{ id: "file_archive", name: "File archive", message: "Network error" }])))
      .toBe("File archive could not load: Network error");
  });

  it("counts the rest rather than listing them", () => {
    const message = appRouteErrorMessage(plugins([
      { id: "file_archive", name: "File archive", message: "Network error" },
      { id: "mail_filters", name: "Filters", message: "Boom" }
    ]));
    expect(message).toBe("File archive could not load: Network error (1 other plugin failed too.)");
  });

  // A page the manifest declares but no module registers is a different fault
  // from a module that threw, and the reader is told which one it is.
  it("says so when nothing failed to load but the page is unclaimed", () => {
    expect(appRouteErrorMessage(plugins([]))).toBe("The plugin that provides this page is enabled but did not register it.");
  });
});
