// File overview: The recipe box's API, as the views read it.

export type StepSection = { name: string; steps: string[] };

export type RecipeSummary = {
  id: number;
  title: string;
  description: string;
  site_name: string;
  url: string;
  yield: string;
  total_minutes: number;
  rating_value: number;
  rating_count: number;
  categories: string[];
  cuisines: string[];
  keywords: string[];
  ingredient_count: number;
  image_url: string;
  note: string;
  origin: "structured" | "manual" | string;
  dismissed: boolean;
  first_seen_at: number;
};

export type RecipeMail = {
  message_id: number;
  subject: string;
  date_unix: number;
  note: string;
  move_state: string;
  move_error: string;
};

export type RecipeDetail = RecipeSummary & {
  author: string;
  prep_minutes: number;
  cook_minutes: number;
  calories: string;
  ingredients: string[];
  instructions: StepSection[];
  mails: RecipeMail[];
};

export type LinkSummary = {
  id: number;
  url: string;
  final_url: string;
  status: "pending" | "page" | "failed" | string;
  error: string;
  attempts: number;
  next_attempt_at: number;
  title: string;
  description: string;
  image_url: string;
  site_name: string;
  subject: string;
  date_unix: number;
  message_id: number;
};

export type Settings = {
  enabled: boolean;
  own_recipients_only: boolean;
  move_enabled: boolean;
  folder_name: string;
  scan_started_at: number;
  scan_finished_at: number;
};

export type FolderStatus = {
  account_id: number;
  account: string;
  mailbox_id: number;
  name: string;
  show_in_all_mail: boolean;
};

export type Status = {
  messages_total: number;
  messages_scanned: number;
  notes_found: number;
  links: Record<string, number>;
  recipes: number;
  mails: Record<string, number>;
  folders: FolderStatus[];
};

export type SettingsResponse = { settings: Settings; status: Status };

const base = "/api/plugins/recipe_box";

export const recipeAPI = {
  settings: () => getJSON<SettingsResponse>(`${base}/settings`),
  saveSettings: (csrf: string, settings: Partial<Settings>) => postJSON<SettingsResponse>(`${base}/settings`, csrf, settings),
  restartScan: (csrf: string) => postJSON<SettingsResponse>(`${base}/scan/restart`, csrf, {}),
  hideFolder: (csrf: string, mailboxID: number) => postJSON<SettingsResponse>(`${base}/folders/hide`, csrf, { mailbox_id: mailboxID }),
  recipes: (query: string, view: "" | "dismissed") => {
    const params = new URLSearchParams();
    if (query.trim()) params.set("q", query.trim());
    if (view) params.set("view", view);
    const suffix = params.toString();
    return getJSON<{ recipes: RecipeSummary[] }>(`${base}/recipes${suffix ? `?${suffix}` : ""}`);
  },
  recipe: (id: number) => getJSON<{ recipe: RecipeDetail }>(`${base}/recipes/${id}`),
  setDismissed: (csrf: string, id: number, dismissed: boolean) => postJSON<{ recipe: RecipeDetail }>(`${base}/recipes/${id}`, csrf, { dismissed }),
  addRecipe: (csrf: string, url: string) => postJSON<{ ok: boolean; status: string; recipe_id: number }>(`${base}/recipes`, csrf, { url }),
  links: (status: "page" | "failed" | "pending") => getJSON<{ links: LinkSummary[] }>(`${base}/links?status=${status}`),
  promoteLink: (csrf: string, id: number) => postJSON<{ ok: boolean }>(`${base}/links/${id}/recipe`, csrf, {}),
  retryLink: (csrf: string, id: number) => postJSON<{ ok: boolean }>(`${base}/links/${id}/retry`, csrf, {})
};

async function getJSON<T>(url: string): Promise<T> {
  const res = await fetch(url, { credentials: "same-origin" });
  if (!res.ok) throw new Error(await errorText(res));
  return res.json();
}

async function postJSON<T>(url: string, csrf: string, payload: unknown): Promise<T> {
  const res = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
    body: JSON.stringify(payload)
  });
  if (!res.ok) throw new Error(await errorText(res));
  return res.json();
}

async function errorText(res: Response) {
  const text = (await res.text().catch(() => "")).trim();
  try {
    const data = JSON.parse(text);
    return data.error || data.message || res.statusText;
  } catch {
    return text || res.statusText;
  }
}

export function messageFromError(err: unknown) {
  if (err instanceof TypeError) return "The server could not be reached.";
  return err instanceof Error ? err.message : "Request failed";
}

/** minutesLabel prints a cooking time the way a cookbook does. */
export function minutesLabel(minutes: number): string {
  if (!minutes || minutes <= 0) return "";
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  return rest ? `${hours} h ${rest} min` : `${hours} h`;
}

/** hostOf names the site a link belongs to, for a recipe whose page named none. */
export function hostOf(link: string): string {
  try {
    return new URL(link).hostname.replace(/^www\./, "");
  } catch {
    return "";
  }
}
