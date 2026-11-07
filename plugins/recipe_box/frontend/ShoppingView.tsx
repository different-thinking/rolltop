// File overview: The shopping list -- what the reader picked out of their
// recipes, and what else they wrote down to buy.
//
// What is still to buy is grouped by where it came from: the reader's own
// items first, then each recipe in the order it reached the list, with the
// recipe's sections kept. Ticking an item off moves it into the cart below,
// where it stays until the reader clears it, so a mis-tap in the shop is one
// tap to undo. The list lives on the server, so the phone in the shop shows
// what was picked on the laptop at home; it is read again whenever the page
// comes back into view.

import { useEffect, useMemo, useRef, useState } from "react";
import type { FormEvent } from "react";
import { Icon } from "../../../frontend/src/components/Icon";
import type { RuntimeAppViewContext } from "../../../frontend/src/plugins/runtime";
import { messageFromError, recipeAPI, type ShoppingItem } from "./api";
import { recipesPath, useCurrentLoad, useLink } from "./RecipesView";

type ItemGroup = {
  key: string;
  recipeID: number;
  title: string;
  sections: { name: string; items: ShoppingItem[] }[];
};

/**
 * groupShopping sorts the items still to buy by where they came from: the
 * reader's own first, then one group per recipe in the order its first item
 * was added, each with its sections in the order they appeared.
 */
export function groupShopping(items: ShoppingItem[]): ItemGroup[] {
  const groups = new Map<string, ItemGroup>();
  const own: ItemGroup = { key: "own", recipeID: 0, title: "", sections: [] };
  for (const item of items) {
    if (item.checked_at) continue;
    let group = own;
    if (item.recipe_id > 0) {
      const key = `recipe-${item.recipe_id}`;
      group = groups.get(key) || { key, recipeID: item.recipe_id, title: item.recipe_title, sections: [] };
      groups.set(key, group);
      // The newest copy of the recipe's name wins: it is written on each add.
      if (item.recipe_title) group.title = item.recipe_title;
    }
    let section = group.sections.find((entry) => entry.name === item.section);
    if (!section) {
      section = { name: item.section, items: [] };
      group.sections.push(section);
    }
    section.items.push(item);
  }
  return (own.sections.length ? [own] : []).concat(Array.from(groups.values()));
}

export function ShoppingView({ csrf, navigate, addToast, mailGeneration }: RuntimeAppViewContext) {
  const [items, setItems] = useState<ShoppingItem[] | null>(null);
  const [error, setError] = useState("");
  const [text, setText] = useState("");
  const [adding, setAdding] = useState(false);
  const [busyIDs, setBusyIDs] = useState<Set<number>>(() => new Set());
  const [visible, setVisible] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const link = useLink(navigate);

  // Back in view -- the phone unlocked in the shop -- the list is read again,
  // so what was added elsewhere in the meantime is there.
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState === "visible") setVisible((value) => value + 1);
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => document.removeEventListener("visibilitychange", onVisible);
  }, []);

  useCurrentLoad(
    "shopping",
    `${mailGeneration}:${visible}`,
    () => recipeAPI.shopping(),
    (next) => {
      setItems(next.items);
      setError("");
    },
    (err) => setError(messageFromError(err)),
  );

  const groups = useMemo(() => groupShopping(items || []), [items]);
  const cart = useMemo(() => (items || []).filter((item) => item.checked_at).sort((a, b) => b.checked_at - a.checked_at || b.id - a.id), [items]);
  const open = (items || []).length - cart.length;

  async function run(ids: number[], change: Promise<{ items: ShoppingItem[] }>, done?: string) {
    setBusyIDs((current) => new Set([...current, ...ids]));
    try {
      const next = await change;
      setItems(next.items);
      if (done) addToast(done);
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusyIDs((current) => {
        const next = new Set(current);
        for (const id of ids) next.delete(id);
        return next;
      });
    }
  }

  function setChecked(item: ShoppingItem, checked: boolean) {
    // Ticked at once: in the shop a box that waits for the network is a box
    // tapped twice. The server's answer replaces the guess.
    setItems((current) => (current || []).map((entry) => (entry.id === item.id ? { ...entry, checked_at: checked ? Math.floor(Date.now() / 1000) : 0 } : entry)));
    void run([item.id], recipeAPI.setShoppingChecked(csrf, item.id, checked));
  }

  async function add(event: FormEvent) {
    event.preventDefault();
    if (!text.trim()) return;
    setAdding(true);
    try {
      const next = await recipeAPI.addShoppingItem(csrf, text);
      setItems(next.items);
      setText("");
      inputRef.current?.focus();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setAdding(false);
    }
  }

  function clearAll() {
    if (!window.confirm("Take every item off the shopping list?")) return;
    void run([], recipeAPI.clearShopping(csrf, true), "The shopping list is empty.");
  }

  function row(item: ShoppingItem) {
    const busy = busyIDs.has(item.id);
    return (
      <li key={item.id} className={item.checked_at ? "is-checked" : ""}>
        <label>
          <input type="checkbox" checked={Boolean(item.checked_at)} disabled={busy} onChange={() => setChecked(item, !item.checked_at)} />
          <span>{item.text}</span>
        </label>
        {item.checked_at && item.recipe_title ? <small className="muted">{item.recipe_title}</small> : null}
        <button
          className="shopping-remove"
          type="button"
          title="Remove from the list"
          aria-label={`Remove ${item.text} from the list`}
          disabled={busy}
          onClick={() => void run([item.id], recipeAPI.deleteShoppingItem(csrf, item.id))}
        >
          <Icon name="close" />
        </button>
      </li>
    );
  }

  return (
    <div className="recipe-box shopping-list">
      <div className="content-head recipe-box-head">
        <div>
          <h1>Shopping list</h1>
          {items ? <span className="label-pill">{open.toLocaleString()} to buy</span> : null}
        </div>
        <div className="recipe-box-head-actions">
          <a className="button secondary" href={recipesPath} onClick={link(recipesPath)}><Icon name="cooking_pot" />Recipes</a>
          {items && items.length > 0 ? (
            <button className="secondary" type="button" onClick={clearAll} disabled={busyIDs.size > 0}><Icon name="delete" />Clear list</button>
          ) : null}
        </div>
      </div>

      <form className="panel shopping-add" onSubmit={add}>
        <input
          ref={inputRef}
          type="text"
          value={text}
          maxLength={300}
          onChange={(event) => setText(event.target.value)}
          placeholder="Add an item, e.g. washing-up liquid"
          aria-label="Add an item to the shopping list"
        />
        <button type="submit" disabled={adding || !text.trim()}><Icon name="add" />Add</button>
      </form>

      {error ? <div className="error">{error}</div> : null}
      {items === null && !error ? <p className="muted">Loading…</p> : null}
      {items !== null && items.length === 0 ? (
        <p className="muted recipe-empty">
          The shopping list is empty. Open a recipe and choose <strong>Shopping list</strong> beside its ingredients, or write an item above.
        </p>
      ) : null}
      {items !== null && items.length > 0 && open === 0 ? <p className="muted recipe-empty">Everything is in the cart.</p> : null}

      {groups.map((group) => (
        <section className="panel shopping-group" key={group.key}>
          <h2>
            {group.recipeID ? (
              <a href={`${recipesPath}/${group.recipeID}`} onClick={link(`${recipesPath}/${group.recipeID}`)}>
                <Icon name="cooking_pot" />{group.title || "Recipe"}
              </a>
            ) : <><Icon name="edit" />My items</>}
          </h2>
          {group.sections.map((section) => (
            <div key={section.name || "-"}>
              {section.name ? <h3>{section.name}</h3> : null}
              <ul className="shopping-items">{section.items.map(row)}</ul>
            </div>
          ))}
        </section>
      ))}

      {cart.length > 0 ? (
        <section className="panel shopping-group shopping-cart">
          <div className="shopping-cart-head">
            <h2><Icon name="check" />In the cart <span className="recipe-tab-count">{cart.length.toLocaleString()}</span></h2>
            <button
              className="secondary"
              type="button"
              disabled={busyIDs.size > 0}
              onClick={() => void run([], recipeAPI.clearShopping(csrf, false), "Cleared what you bought.")}
            >
              Clear bought items
            </button>
          </div>
          <ul className="shopping-items">{cart.map(row)}</ul>
        </section>
      ) : null}
    </div>
  );
}
