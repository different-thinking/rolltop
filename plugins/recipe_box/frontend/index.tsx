// File overview: Runtime frontend for the recipe box plugin: the cookbook and
// the notes overview under the sidebar's Organizer heading, and the settings
// page they share.

import type { AccountSettingsRouteContext, RuntimeAppViewContext } from "../../../frontend/src/plugins/runtime";
import { NotesView } from "./NotesView";
import { RecipeSettings } from "./RecipeSettings";
import { notesPath, RecipesView, recipesPath, settingsPath } from "./RecipesView";
import "./styles.css";

export default {
  appViews: [
    {
      path: recipesPath,
      label: "Recipes",
      icon: "cooking_pot",
      render: (context: RuntimeAppViewContext) => <RecipesView {...context} />
    },
    {
      path: notesPath,
      label: "Notes",
      icon: "draft",
      render: (context: RuntimeAppViewContext) => <NotesView {...context} />
    }
  ],
  accountSettingsRoutes: [
    {
      path: settingsPath,
      title: "Recipe box",
      label: "Recipes",
      description: "Collect the recipes and notes you mail yourself, and file those mails out of the Inbox.",
      icon: "cooking_pot",
      section: "plugins",
      render: (context: AccountSettingsRouteContext) => <RecipeSettings {...context} />
    }
  ]
};
