import { describe, expect, it } from "vitest";

import { mailRouteView } from "../lib/routes";
import { appViews, matchAppView, type RuntimePlugin } from "./runtime";

const render = () => null;

describe("appViews", () => {
  it("keeps plugin pages inside the claimed subtree and refuses a second claim on one path", () => {
    const plugins: RuntimePlugin[] = [
      { appViews: [{ path: "/apps/recipes/", label: "Recipes", icon: "cooking_pot", render }] },
      // A page outside /apps would be mail to the router and a plugin page to
      // the sidebar; a second claim on one path would make plugin order decide.
      { appViews: [{ path: "/mail/recipes", label: "Mail", icon: "mail", render }, { path: "/apps/recipes", label: "Again", icon: "x", render }] },
      { appViews: "not a list" }
    ];
    const views = appViews(plugins);
    expect(views.map((view) => [view.path, view.label])).toEqual([["/apps/recipes", "Recipes"]]);
  });

  it("matches a page and the routes below it, and nothing beside it", () => {
    const plugins: RuntimePlugin[] = [{ appViews: [{ path: "/apps/recipes", label: "Recipes", icon: "cooking_pot", render }] }];
    expect(matchAppView(plugins, "/apps/recipes")?.label).toBe("Recipes");
    expect(matchAppView(plugins, "/apps/recipes/12")?.label).toBe("Recipes");
    expect(matchAppView(plugins, "/apps/recipesx")).toBeNull();
  });

  it("is never mail, whether or not a plugin answers for it yet", () => {
    expect(mailRouteView("/apps/recipes", false)).toBe(false);
    expect(mailRouteView("/apps/unknown/1", false)).toBe(false);
    expect(mailRouteView("/application", false)).toBe(true);
  });
});
