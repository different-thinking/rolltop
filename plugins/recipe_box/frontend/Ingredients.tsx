// File overview: A recipe's ingredients, and choosing which of them go on the
// shopping list.
//
// The panel has two modes. Cooking, the default, ticks ingredients off while
// the dish is made; those ticks live only on this page. Shopping, switched on
// with "Shopping list", turns the boxes into a choice of what to buy: one by
// one, a whole section, or every ingredient at once. What is on the list
// already is marked in both modes, so a second recipe shows what the first one
// put there.

import { useMemo, useState } from "react";
import type { MouseEvent } from "react";
import { Icon } from "../../../frontend/src/components/Icon";
import type { AddToast } from "../../../frontend/src/appTypes";
import { messageFromError, recipeAPI, type IngredientSection, type RecipeDetail, type ShoppingItem } from "./api";

type Props = {
  recipe: RecipeDetail;
  csrf: string;
  addToast: AddToast;
  /** The reader's shopping list, null while it has not loaded. */
  shopping: ShoppingItem[] | null;
  onShoppingChange: (items: ShoppingItem[]) => void;
  shoppingHref: string;
  onOpenShopping: (event: MouseEvent) => void;
  /** Called once ingredients went on the list: the server has planned the dish. */
  onPlanned: () => void;
};

/** sectionsOf reads the recipe's ingredient parts, one unnamed part for a server that sends none. */
export function sectionsOf(recipe: RecipeDetail): IngredientSection[] {
  if (recipe.ingredient_sections) return recipe.ingredient_sections;
  return [{ name: "", items: recipe.ingredients.map((text, index) => ({ index, text })) }];
}

/**
 * shoppingKey is how an item and an ingredient are matched, the way the server
 * matches them: same section, same words, case aside.
 */
export function shoppingKey(section: string, text: string): string {
  const norm = (value: string) => value.trim().replace(/\s+/g, " ").toLowerCase();
  return `${norm(section)}\n${norm(text)}`;
}

/** listedLines is the set of a recipe's lines behind items still to buy, as shoppingKey values. */
function listedLines(items: ShoppingItem[], recipeID: number): Set<string> {
  const keys = new Set<string>();
  for (const item of items) {
    if (item.checked_at) continue;
    for (const source of item.sources || []) {
      if (source.recipe_id === recipeID) keys.add(shoppingKey(source.section, source.text));
    }
  }
  return keys;
}

export function Ingredients({ recipe, csrf, addToast, shopping, onShoppingChange, shoppingHref, onOpenShopping, onPlanned }: Props) {
  const sections = useMemo(() => sectionsOf(recipe), [recipe]);
  const allIndexes = useMemo(() => sections.flatMap((section) => section.items.map((item) => item.index)), [sections]);
  const [cooked, setCooked] = useState<Set<number>>(() => new Set());
  const [picking, setPicking] = useState(false);
  const [chosen, setChosen] = useState<Set<number>>(() => new Set());
  const [busy, setBusy] = useState(false);

  // What of this recipe is still to buy: the lines of it behind items not yet
  // ticked off. An item ticked off in the shop is bought, so its lines no
  // longer count as on the list.
  const onList = useMemo(() => listedLines(shopping || [], recipe.id), [shopping, recipe.id]);

  if (allIndexes.length === 0) return null;

  function toggle(set: Set<number>, indexes: number[], on: boolean) {
    const next = new Set(set);
    for (const index of indexes) {
      if (on) next.add(index);
      else next.delete(index);
    }
    return next;
  }

  function startPicking() {
    setChosen(new Set());
    setPicking(true);
  }

  async function add(indexes: number[]) {
    if (indexes.length === 0) return;
    setBusy(true);
    try {
      const before = onList.size;
      const next = await recipeAPI.addIngredients(csrf, recipe.id, indexes);
      onShoppingChange(next.items);
      onPlanned();
      // Counted from the answer: an ingredient on the list already is not
      // added a second time, so the request's size would overstate it.
      const after = listedLines(next.items, recipe.id).size;
      const added = Math.max(0, after - before);
      addToast(added === 0
        ? "Those ingredients are on the shopping list already."
        : added === 1 ? "Added 1 ingredient to the shopping list." : `Added ${added} ingredients to the shopping list.`);
      setPicking(false);
      setChosen(new Set());
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setBusy(false);
    }
  }

  const everyChosen = chosen.size === allIndexes.length;

  return (
    <section className={`recipe-ingredients${picking ? " is-picking" : ""}`}>
      <div className="recipe-ingredients-head">
        <h2>Ingredients</h2>
        {picking ? (
          <button className="secondary" type="button" onClick={() => setChosen(everyChosen ? new Set() : new Set(allIndexes))}>
            {everyChosen ? "Choose none" : "Choose all"}
          </button>
        ) : (
          <button className="secondary" type="button" onClick={startPicking} title="Choose ingredients for the shopping list">
            <Icon name="shopping_bag" />Shopping list
          </button>
        )}
      </div>
      {picking ? <p className="muted recipe-pick-hint">Tick what you need to buy.</p> : null}

      {sections.map((section, sectionIndex) => {
        const indexes = section.items.map((item) => item.index);
        const sectionChosen = indexes.every((index) => chosen.has(index));
        return (
          <div className="recipe-ingredient-section" key={`${sectionIndex}-${section.name}`}>
            {section.name ? (
              <div className="recipe-ingredient-section-head">
                <h3>{section.name}</h3>
                {picking ? (
                  <button className="recipe-link-button" type="button" onClick={() => setChosen(toggle(chosen, indexes, !sectionChosen))}>
                    {sectionChosen ? "Choose none" : "Choose all"}
                  </button>
                ) : null}
              </div>
            ) : null}
            <ul>
              {section.items.map((item) => {
                const listed = onList.has(shoppingKey(section.name, item.text));
                const checked = picking ? chosen.has(item.index) : cooked.has(item.index);
                return (
                  <li key={`${item.index}-${item.text}`}>
                    <label className={!picking && checked ? "done" : ""}>
                      <input
                        type="checkbox"
                        checked={checked}
                        onChange={() => (picking
                          ? setChosen(toggle(chosen, [item.index], !chosen.has(item.index)))
                          : setCooked(toggle(cooked, [item.index], !cooked.has(item.index))))}
                      />
                      <span>{item.text}</span>
                      {listed ? <span className="recipe-on-list" title="On the shopping list"><Icon name="shopping_bag" />On list</span> : null}
                    </label>
                  </li>
                );
              })}
            </ul>
          </div>
        );
      })}

      {picking ? (
        <div className="recipe-pick-actions">
          <button type="button" disabled={busy || chosen.size === 0} onClick={() => void add(allIndexes.filter((index) => chosen.has(index)))}>
            <Icon name="add" />{chosen.size ? `Add ${chosen.size} to the list` : "Add to the list"}
          </button>
          <button className="secondary" type="button" disabled={busy} onClick={() => setPicking(false)}>Cancel</button>
        </div>
      ) : onList.size > 0 ? (
        <p className="muted recipe-pick-footer">
          <a href={shoppingHref} onClick={onOpenShopping}>
            <Icon name="shopping_bag" />{onList.size === 1 ? "1 ingredient" : `${onList.size} ingredients`} on the shopping list
          </a>
        </p>
      ) : null}
    </section>
  );
}
