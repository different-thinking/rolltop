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
  /** When the reader marked the dish cooked; zero while it is still one to try. */
  cooked_at: number;
  /**
   * When ingredients of the dish last went on the shopping list; zero once it
   * is cooked or taken out of the plan. Planned dishes lead the cookbook.
   */
  planned_at: number;
};

export type RecipeView = "" | "cooked" | "dismissed";

/** `planned` counts the dishes among `to_cook` whose ingredients are being shopped for. */
export type RecipeCounts = { to_cook: number; cooked: number; dismissed: number; planned: number };

export type RecipeMail = {
  message_id: number;
  subject: string;
  date_unix: number;
  note: string;
  move_state: string;
  move_error: string;
};

/** One ingredient and its index into the recipe's stored list. */
export type IngredientLine = { index: number; text: string };

/**
 * One part of a recipe's ingredients ("For the dough"). A list without
 * headings is one section without a name.
 */
export type IngredientSection = { name: string; items: IngredientLine[] };

export type RecipeDetail = RecipeSummary & {
  author: string;
  prep_minutes: number;
  cook_minutes: number;
  calories: string;
  ingredients: string[];
  ingredient_sections: IngredientSection[];
  instructions: StepSection[];
  mails: RecipeMail[];
};

/** The shelves the notes overview sorts by, in the order it draws them. */
export type NoteKind =
  | "text" | "article" | "video" | "product" | "place" | "event"
  | "music" | "media" | "tech" | "social" | "docs" | "link";

export type NoteLink = {
  id: number;
  url: string;
  final_url: string;
  status: "pending" | "page" | "failed" | "recipe" | string;
  kind: string;
  /** The one detail a card shows for its kind: a price, an event's date. */
  detail: string;
  title: string;
  description: string;
  site_name: string;
  error: string;
};

export type Note = {
  id: number;
  message_id: number;
  subject: string;
  date_unix: number;
  body: string;
  kind: NoteKind | string;
  done_at: number;
  filed: boolean;
  links: NoteLink[];
};

/** One recipe line, or one line the reader typed in, behind a shopping item. */
export type ShoppingSource = {
  /** The recipe the line came from, zero for one the reader typed in. */
  recipe_id: number;
  recipe_title: string;
  section: string;
  /** The line as the recipe (or the reader) wrote it. */
  text: string;
};

/**
 * One line of the shopping list: the same thing to buy stands there once,
 * its amounts added up across the recipes it came from.
 */
export type ShoppingItem = {
  id: number;
  name: string;
  /** The total in `unit`, null when the lines named no amount ("Salz"). Weights are in grams, volumes in millilitres. */
  amount: number | null;
  unit: string;
  /** When the reader ticked it off, zero while it is still to buy. */
  checked_at: number;
  created_at: number;
  sources: ShoppingSource[];
};

export type ShoppingResponse = { items: ShoppingItem[] };

export type NotesResponse = {
  notes: Note[];
  counts: Record<string, number>;
  totals: { open: number; done: number };
};

export type Settings = {
  enabled: boolean;
  own_recipients_only: boolean;
  move_enabled: boolean;
  folder_name: string;
  notes_move_enabled: boolean;
  notes_folder_name: string;
  scan_started_at: number;
  scan_finished_at: number;
};

export type FolderStatus = {
  shelf: "recipes" | "notes" | string;
  wanted: string;
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
  notes: number;
  /** Items on the shopping list still to buy. */
  shopping: number;
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
  recipes: (query: string, view: RecipeView) => {
    const params = new URLSearchParams();
    if (query.trim()) params.set("q", query.trim());
    if (view) params.set("view", view);
    const suffix = params.toString();
    return getJSON<{ recipes: RecipeSummary[]; counts: RecipeCounts }>(`${base}/recipes${suffix ? `?${suffix}` : ""}`);
  },
  recipe: (id: number) => getJSON<{ recipe: RecipeDetail }>(`${base}/recipes/${id}`),
  setDismissed: (csrf: string, id: number, dismissed: boolean) => postJSON<{ recipe: RecipeDetail }>(`${base}/recipes/${id}`, csrf, { dismissed }),
  setCooked: (csrf: string, id: number, cooked: boolean) => postJSON<{ recipe: RecipeDetail }>(`${base}/recipes/${id}`, csrf, { cooked }),
  /** Takes a dish out of the plan. It is planned by putting its ingredients on the shopping list. */
  unplan: (csrf: string, id: number) => postJSON<{ recipe: RecipeDetail }>(`${base}/recipes/${id}`, csrf, { planned: false }),
  addRecipe: (csrf: string, url: string) => postJSON<{ ok: boolean; status: string; recipe_id: number }>(`${base}/recipes`, csrf, { url }),
  notes: (state: "open" | "done", query: string) => {
    const params = new URLSearchParams({ state });
    if (query.trim()) params.set("q", query.trim());
    return getJSON<NotesResponse>(`${base}/notes?${params.toString()}`);
  },
  setNoteDone: (csrf: string, id: number, done: boolean) => postJSON<{ ok: boolean }>(`${base}/notes/${id}`, csrf, { done }),
  dismissNote: (csrf: string, id: number) => postJSON<{ ok: boolean }>(`${base}/notes/${id}`, csrf, { dismissed: true }),
  /** Moves the mail behind a note to its account's Trash and takes the note out of the overview. */
  deleteNote: (csrf: string, id: number) => postJSON<{ ok: boolean; moved: number }>(`${base}/notes/${id}/delete`, csrf, {}),
  shopping: () => getJSON<ShoppingResponse>(`${base}/shopping`),
  /** Puts ingredients of a recipe on the list, named by their index into its ingredient list. */
  addIngredients: (csrf: string, recipeID: number, indexes: number[]) => postJSON<ShoppingResponse>(`${base}/shopping`, csrf, { recipe_id: recipeID, indexes }),
  /** Puts an item of the reader's own on the list. */
  addShoppingItem: (csrf: string, text: string) => postJSON<ShoppingResponse>(`${base}/shopping`, csrf, { text }),
  setShoppingChecked: (csrf: string, id: number, checked: boolean) => postJSON<ShoppingResponse>(`${base}/shopping/${id}`, csrf, { checked }),
  deleteShoppingItem: (csrf: string, id: number) => postJSON<ShoppingResponse>(`${base}/shopping/${id}/delete`, csrf, {}),
  /** Takes the ticked-off items off the list, or with `all` every item. */
  clearShopping: (csrf: string, all: boolean) => postJSON<ShoppingResponse>(`${base}/shopping/clear`, csrf, { all }),
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

const fractionGlyphs: [number, string][] = [[1 / 4, "¼"], [1 / 3, "⅓"], [1 / 2, "½"], [2 / 3, "⅔"], [3 / 4, "¾"]];

/**
 * shoppingAmount prints an item's total the way a shopping list says it:
 * grams and millilitres from a thousand up as kilograms and litres, decimals
 * in the reader's own notation, and a spoon or a cup as a fraction ("½ TL").
 */
export function shoppingAmount(item: Pick<ShoppingItem, "amount" | "unit">): string {
  if (item.amount === null || item.amount === undefined) return "";
  let amount = item.amount;
  let unit = item.unit;
  if (unit === "g" && amount >= 1000) { amount /= 1000; unit = "kg"; }
  else if (unit === "ml" && amount >= 1000) { amount /= 1000; unit = "l"; }
  let number = "";
  if (unit !== "g" && unit !== "ml" && unit !== "kg" && unit !== "l") {
    const whole = Math.floor(amount + 1e-9);
    const glyph = fractionGlyphs.find(([value]) => Math.abs(amount - whole - value) < 0.01);
    if (glyph) number = `${whole > 0 ? whole : ""}${glyph[1]}`;
  }
  if (!number) number = amount.toLocaleString(undefined, { maximumFractionDigits: 2 });
  return unit ? `${number} ${unit}` : number;
}

/** shoppingRecipes names the recipes an item was added up from, each once. */
export function shoppingRecipes(item: ShoppingItem): string[] {
  return Array.from(new Set((item.sources || []).filter((source) => source.recipe_id > 0).map((source) => source.recipe_title || "Recipe")));
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
