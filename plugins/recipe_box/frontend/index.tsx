// File overview: Runtime frontend for the recipe box plugin: the cookbook page
// under the sidebar's Organizer heading, and its settings page.

import type { AccountSettingsRouteContext, RuntimeAppViewContext } from "../../../frontend/src/plugins/runtime";
import { RecipeSettings } from "./RecipeSettings";
import { RecipesView, recipesPath, settingsPath } from "./RecipesView";
import "./styles.css";

export default {
  appViews: [
    {
      path: recipesPath,
      label: "Recipes",
      icon: "cooking_pot",
      render: (context: RuntimeAppViewContext) => <RecipesView {...context} />
    }
  ],
  accountSettingsRoutes: [
    {
      path: settingsPath,
      title: "Recipe box",
      label: "Recipes",
      description: "Collect the recipes you mail yourself, and file those mails out of the Inbox.",
      icon: "cooking_pot",
      section: "plugins",
      render: (context: AccountSettingsRouteContext) => <RecipeSettings {...context} />
    }
  ]
};
