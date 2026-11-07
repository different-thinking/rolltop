// File overview: The shopping list -- one list of what to buy, picked out of
// the reader's recipes and typed in by hand.
//
// The same thing to buy stands on it once, its amounts added up by the server
// across every recipe it came from ("800 g Tomaten"); under each item the
// recipes it was added up from. Ticking an item off moves it into the cart
// below, where it stays until the reader clears it, so a mis-tap in the shop
// is one tap to undo. The list lives on the server, so the phone in the shop
// shows what was picked on the laptop at home; it is read again whenever the
// page comes back into view.

import { useEffect, useMemo, useRef, useState } from "react";
import type { FormEvent } from "react";
import { Icon } from "../../../frontend/src/components/Icon";
import type { RuntimeAppViewContext } from "../../../frontend/src/plugins/runtime";
import { messageFromError, recipeAPI, shoppingAmount, shoppingRecipes, type ShoppingItem } from "./api";
import { recipesPath, useCurrentLoad, useLink } from "./RecipesView";

/** shoppingLabel is an item as one line: "800 g Tomaten", "3 Eier", "Salz". */
export function shoppingLabel(item: ShoppingItem): string {
  const amount = shoppingAmount(item);
  return amount ? `${amount} ${item.name}` : item.name;
}

export function ShoppingView({ csrf, navigate, addToast, mailGeneration }: RuntimeAppViewContext) {
  const [items, setItems] = useState<ShoppingItem[] | null>(null);
  const [error, setError] = useState("");
  const [text, setText] = useState("");
  const [adding, setAdding] = useState(false);
  const [busyIDs, setBusyIDs] = useState<Set<number>>(() => new Set());
  const [visible, setVisible] = useState(0);
  const [clearing, setClearing] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  // The latest change sent. Each answer carries the whole list, so only the
  // answer to the latest change may draw it: an earlier one arriving later
  // would undo a tick made in between.
  const latestChange = useRef(0);
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

  const toBuy = useMemo(() => (items || []).filter((item) => !item.checked_at), [items]);
  const cart = useMemo(() => (items || []).filter((item) => item.checked_at).sort((a, b) => b.checked_at - a.checked_at || b.id - a.id), [items]);
  const open = (items || []).length - cart.length;

  async function run(ids: number[], change: () => Promise<{ items: ShoppingItem[] }>, done?: string) {
    const seq = ++latestChange.current;
    setBusyIDs((current) => new Set([...current, ...ids]));
    try {
      const next = await change();
      if (seq === latestChange.current) setItems(next.items);
      if (done) addToast(done);
    } catch (err) {
      addToast(messageFromError(err), "error");
      // A tick drawn ahead of a change that failed must not stay drawn: the
      // list is read again for what the server really holds.
      setVisible((value) => value + 1);
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
    void run([item.id], () => recipeAPI.setShoppingChecked(csrf, item.id, checked));
  }

  async function add(event: FormEvent) {
    event.preventDefault();
    if (!text.trim()) return;
    setAdding(true);
    const seq = ++latestChange.current;
    try {
      const next = await recipeAPI.addShoppingItem(csrf, text);
      if (seq === latestChange.current) setItems(next.items);
      setText("");
      inputRef.current?.focus();
    } catch (err) {
      addToast(messageFromError(err), "error");
      // An earlier change's answer was dropped as superseded by this one, so
      // with this one failed nothing has drawn the list since: read it again.
      setVisible((value) => value + 1);
    } finally {
      setAdding(false);
    }
  }

  async function clear(all: boolean) {
    if (all && !window.confirm("Take every item off the shopping list?")) return;
    setClearing(true);
    try {
      await run([], () => recipeAPI.clearShopping(csrf, all), all ? "The shopping list is empty." : "Cleared what you bought.");
    } finally {
      setClearing(false);
    }
  }

  function row(item: ShoppingItem) {
    const busy = busyIDs.has(item.id);
    const amount = shoppingAmount(item);
    const recipes = shoppingRecipes(item);
    // The lines as the recipes wrote them, for a reader who wants to know
    // what the total was added up from.
    const lines = (item.sources || []).map((source) => (source.recipe_title ? `${source.recipe_title}: ${source.text}` : source.text)).join("\n");
    return (
      <li key={item.id} className={item.checked_at ? "is-checked" : ""}>
        <label title={lines}>
          <input type="checkbox" checked={Boolean(item.checked_at)} disabled={busy} onChange={() => setChecked(item, !item.checked_at)} />
          <span className="shopping-item-text">
            <span>{amount ? <strong className="shopping-amount">{amount}</strong> : null}{amount ? " " : ""}{item.name}</span>
            {recipes.length > 0 ? <small className="muted shopping-recipes">{recipes.join(" · ")}</small> : null}
          </span>
        </label>
        <button
          className="shopping-remove"
          type="button"
          title="Remove from the list"
          aria-label={`Remove ${shoppingLabel(item)} from the list`}
          disabled={busy}
          onClick={() => void run([item.id], () => recipeAPI.deleteShoppingItem(csrf, item.id))}
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
            <button className="secondary" type="button" onClick={() => void clear(true)} disabled={clearing || busyIDs.size > 0}><Icon name="delete" />Clear list</button>
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

      {toBuy.length > 0 ? (
        <section className="panel shopping-group">
          <ul className="shopping-items">{toBuy.map(row)}</ul>
        </section>
      ) : null}

      {cart.length > 0 ? (
        <section className="panel shopping-group shopping-cart">
          <div className="shopping-cart-head">
            <h2><Icon name="check" />In the cart <span className="recipe-tab-count">{cart.length.toLocaleString()}</span></h2>
            <button
              className="secondary"
              type="button"
              disabled={clearing || busyIDs.size > 0}
              onClick={() => void clear(false)}
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
