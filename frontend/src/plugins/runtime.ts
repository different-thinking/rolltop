import { Fragment, createElement } from "react";
import type { ReactNode } from "react";
import type { AddToast, DatePrefs, LocationState, Navigate } from "../appTypes";
import type { SettingsSectionID } from "../features/settings/SettingsUI";
import type { FrontendPluginDefinition, Mailbox, Message, MessageAnnotation, User } from "../types";
import { appViewPrefix } from "../lib/routes";
import { syncPluginStylesheets } from "./pluginStyles";

export type RuntimePluginModule = {
  default?: unknown;
};

export type RuntimePlugin = Record<string, unknown>;

export type AccountSettingsRouteContext = {
  csrf: string;
  user: User;
  mailboxes: Mailbox[];
  location: LocationState;
  navigate: Navigate;
  addToast: AddToast;
};

export type AccountSettingsRoute = {
  /** path is the canonical URL used by settings navigation. */
  path: string;
  /** aliases preserve old deep links while callers redirect or render the canonical page. */
  aliases?: readonly string[];
  title: string;
  label: string;
  description: string;
  icon: string;
  section?: SettingsSectionID;
  render: (context: AccountSettingsRouteContext) => ReactNode;
};

export type AccountSettingsRuntimePlugin = RuntimePlugin & {
  accountSettingsRoutes?: readonly AccountSettingsRoute[];
};

/**
 * RuntimeAppViewContext is what a plugin's own page gets. It is the slice of
 * the shell an organizer view such as Parcels is handed: who is reading, how to
 * move and report, and the mail generation, which changes whenever mail does,
 * so a view built from mail can reload on it instead of polling.
 */
export type RuntimeAppViewContext = {
  csrf: string;
  user: User;
  mailboxes: Mailbox[];
  location: LocationState;
  mailGeneration: number;
  datePrefs: DatePrefs;
  navigate: Navigate;
  addToast: AddToast;
  /**
   * messageFiledTo names the folder the reader has just filed a message into
   * (`filedMessageDestination`), 0 when they have not. A page drawing
   * something built from a message can leave it out the moment the reader
   * deletes the mail elsewhere, rather than for the undo window and the move
   * behind it. The function is replaced whenever a filing changes, so a page
   * memoizing on it recomputes then.
   */
  messageFiledTo: (messageID: number) => number;
};

/**
 * RuntimeAppView is a page of a plugin's own, listed under the sidebar's
 * Organizer heading beside Parcels and Invoices. Its path lives under
 * `appViewPrefix` (`/apps/...`) and is claimed with everything below it, so a
 * view can grow detail routes without a second registration.
 */
export type RuntimeAppView = {
  path: string;
  label: string;
  icon: string;
  render: (context: RuntimeAppViewContext) => ReactNode;
};

export type RuntimeAppViewPlugin = RuntimePlugin & {
  appViews?: readonly RuntimeAppView[];
};

/** appViews returns every plugin page whose path lies inside the claimed subtree, in plugin order. */
export function appViews(plugins: RuntimePlugins | readonly RuntimePlugin[]): RuntimeAppView[] {
  const list: readonly RuntimePlugin[] = Array.isArray(plugins) ? plugins : (plugins as RuntimePlugins).all;
  const seen = new Set<string>();
  return (list as readonly RuntimeAppViewPlugin[]).flatMap((plugin) => {
    const views = Array.isArray(plugin.appViews) ? plugin.appViews : [];
    return views.flatMap((view) => {
      if (!view || typeof view.render !== "function" || typeof view.path !== "string") return [];
      const path = normalizedSettingsPath(view.path);
      // A path outside the subtree would be mail to the router and a plugin
      // page to the sidebar; two plugins claiming one path would make plugin
      // order decide which one renders. Both are refused rather than guessed.
      if (!path.startsWith(`${appViewPrefix}/`) || seen.has(path)) return [];
      seen.add(path);
      return [{
        path,
        label: cleanSettingsText(view.label, settingsLabelFromPath(path)),
        icon: cleanSettingsText(view.icon, "folder"),
        render: view.render
      }];
    });
  });
}

/** matchAppView finds the plugin page serving a path, its own subtree included. */
export function matchAppView(plugins: RuntimePlugins | readonly RuntimePlugin[], path: string): RuntimeAppView | null {
  const candidate = normalizedSettingsPath(path);
  return appViews(plugins).find((view) => candidate === view.path || candidate.startsWith(`${view.path}/`)) || null;
}

/** RuntimeMessageDetailsContext is the stable, read-only input for plugin rows
 * rendered inside a message's expanded header details. */
export type RuntimeMessageDetailsContext = {
  message: Message;
  annotations: readonly MessageAnnotation[];
  datePrefs: DatePrefs;
};

/** RuntimeMessageDetailsPlugin contributes dt/dd rows to the message details list. */
export type RuntimeMessageDetailsPlugin = RuntimePlugin & {
  renderMessageDetails?: (context: RuntimeMessageDetailsContext) => ReactNode;
};

/**
 * RuntimeMessageQuickActionContext is what an icon-only button sitting beside
 * Reply, Archive and Delete gets. The toolbar it joins owns the look, so it
 * hands the plugin the class its own buttons wear and whether they are
 * currently accepting clicks -- a plugin button that stayed live while the row
 * was being moved would act on a message that has already left the list.
 */
export type RuntimeMessageQuickActionContext = {
  message: Message;
  location: "message-list" | "thread";
  buttonClassName: string;
  disabled: boolean;
  navigate: Navigate;
  addToast: AddToast;
};

/** RuntimeMessageQuickActionPlugin contributes buttons to a message's action toolbar. */
export type RuntimeMessageQuickActionPlugin = RuntimePlugin & {
  renderMessageQuickActions?: (context: RuntimeMessageQuickActionContext) => ReactNode;
};

/**
 * messageQuickActionNodes renders the hook for every plugin that answers it.
 * Both toolbars carrying these buttons -- a list row and an open conversation
 * -- call it, so it lives beside the hook it reads rather than as a copy in
 * each view: the keys, the plugin order and the empty-node filter are one
 * decision about this hook, not two that can drift apart.
 */
export function messageQuickActionNodes(
  plugins: readonly RuntimePlugin[],
  context: RuntimeMessageQuickActionContext
): ReactNode[] {
  return (plugins as readonly RuntimeMessageQuickActionPlugin[])
    .map((plugin, index) => {
      const node = plugin.renderMessageQuickActions?.(context);
      return node ? createElement(Fragment, { key: `message-quick-action-${index}` }, node) : null;
    })
    .filter((node) => node !== null);
}

export type RuntimePlugins = {
  all: RuntimePlugin[];
  byID: Record<string, RuntimePlugin>;
  status: "loading" | "ready";
  errors: RuntimePluginLoadError[];
};

export type RuntimePluginLoadError = {
  id: string;
  name: string;
  message: string;
};

export function emptyRuntimePlugins(): RuntimePlugins {
  return { all: [], byID: {}, status: "loading", errors: [] };
}

export async function loadRuntimePlugins(definitions: readonly FrontendPluginDefinition[] = []): Promise<RuntimePlugins> {
  if (typeof document !== "undefined") syncPluginStylesheets(definitions);
  const results = await Promise.all(definitions.map(async (definition) => {
    if (!definition.module_url) {
      return { definition, plugin: null, error: "Plugin module URL is missing." };
    }
    try {
      const mod = await import(/* @vite-ignore */ definition.module_url) as RuntimePluginModule;
      const plugin = normalizeRuntimePlugin(mod.default);
      return plugin
        ? { definition, plugin, error: "" }
        : { definition, plugin: null, error: "Plugin module did not export a runtime plugin." };
    } catch (error) {
      return { definition, plugin: null, error: runtimePluginErrorMessage(error) };
    }
  }));

  // Promise.all preserves input order even when imports finish out of order. Build
  // both lookups afterward so hook rendering and route precedence stay stable.
  const plugins: RuntimePlugins = { all: [], byID: {}, status: "ready", errors: [] };
  results.forEach(({ definition, plugin, error }) => {
    if (plugin) {
      plugins.all.push(plugin);
      plugins.byID[definition.id] = plugin;
      return;
    }
    plugins.errors.push({
      id: definition.id,
      name: definition.name || definition.id,
      message: error || "Plugin failed to load."
    });
  });
  return plugins;
}

export function getRuntimePlugin<T extends RuntimePlugin>(plugins: RuntimePlugins, id: string): T | undefined {
  return plugins.byID[id] as T | undefined;
}

/** accountSettingsRoutes returns normalized plugin pages in deterministic plugin/registration order. */
export function accountSettingsRoutes(plugins: RuntimePlugins | readonly RuntimePlugin[]): AccountSettingsRoute[] {
  const list: readonly RuntimePlugin[] = Array.isArray(plugins) ? plugins : (plugins as RuntimePlugins).all;
  return (list as readonly AccountSettingsRuntimePlugin[]).flatMap((plugin) => {
    const routes = Array.isArray(plugin.accountSettingsRoutes) ? plugin.accountSettingsRoutes : [];
    return routes.flatMap((route) => {
      const normalized = normalizeAccountSettingsRoute(route);
      return normalized ? [normalized] : [];
    });
  });
}

/** matchAccountSettingsRoute matches canonical settings paths and compatibility aliases. */
export function matchAccountSettingsRoute(
  plugins: RuntimePlugins | readonly RuntimePlugin[],
  path: string
): AccountSettingsRoute | null {
  const candidate = normalizedSettingsPath(path);
  return accountSettingsRoutes(plugins).find((route) =>
    normalizedSettingsPath(route.path) === candidate
      || (route.aliases || []).some((alias) => normalizedSettingsPath(alias) === candidate)
  ) || null;
}

function normalizeRuntimePlugin(value: unknown): RuntimePlugin | null {
  if (!value || typeof value !== "object") return null;
  return value as RuntimePlugin;
}

function normalizeAccountSettingsRoute(value: unknown): AccountSettingsRoute | null {
  if (!value || typeof value !== "object") return null;
  const route = value as Partial<AccountSettingsRoute>;
  if (typeof route.path !== "string" || !route.path.trim() || typeof route.render !== "function") return null;
  const path = normalizedSettingsPath(route.path);
  const fallbackLabel = settingsLabelFromPath(path);
  const rawAliases = Array.isArray(route.aliases) ? route.aliases : [];
  const aliases = Array.from(new Set(rawAliases
    .filter((alias): alias is string => typeof alias === "string" && Boolean(alias.trim()))
    .map(normalizedSettingsPath)
    .filter((alias) => alias !== path)));
  return {
    path,
    aliases,
    title: cleanSettingsText(route.title, route.label, fallbackLabel),
    label: cleanSettingsText(route.label, route.title, fallbackLabel),
    description: cleanSettingsText(route.description),
    icon: cleanSettingsText(route.icon, "settings"),
    section: settingsSection(route.section),
    render: route.render
  };
}

function normalizedSettingsPath(value: string): string {
  const path = value.trim().split(/[?#]/, 1)[0] || "/";
  if (path === "/") return path;
  return `/${path.replace(/^\/+|\/+$/g, "")}`;
}

function settingsLabelFromPath(path: string): string {
  const segment = path.split("/").filter(Boolean).at(-1) || "Plugin";
  return segment.split(/[-_]+/).filter(Boolean).map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(" ");
}

function cleanSettingsText(...values: unknown[]): string {
  for (const value of values) {
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

function settingsSection(value: unknown): SettingsSectionID {
  return value === "general" || value === "mail" || value === "preferences" || value === "plugins" ? value : "plugins";
}

function runtimePluginErrorMessage(error: unknown): string {
  if (error instanceof Error && error.message.trim()) return error.message.trim();
  return "Plugin module could not be imported.";
}
