-- A recipe is planned from the moment its ingredients go on the shopping list
-- until the reader marks it cooked. Buying the ingredients does not end the
-- plan -- only cooking does. A cooked recipe whose ingredients go on the list
-- again is planned again and leaves the cooked shelf, so planned_at and
-- cooked_at are never both set.
ALTER TABLE plugin_recipe_box_recipes ADD COLUMN IF NOT EXISTS planned_at BIGINT NOT NULL DEFAULT 0;

-- Recipes with ingredients on the list already are planned as of the latest
-- line added from them. That includes lists the plugin has not folded into
-- sources yet (items with an empty merge_key still carry their recipe). A
-- recipe cooked after its last line was added stays cooked.
UPDATE plugin_recipe_box_recipes r SET planned_at = lines.latest, cooked_at = 0
FROM (
  SELECT user_id, recipe_id, MAX(created_at) AS latest FROM (
    SELECT user_id, recipe_id, created_at FROM plugin_recipe_box_shopping_sources WHERE recipe_id > 0
    UNION ALL
    SELECT user_id, recipe_id, created_at FROM plugin_recipe_box_shopping_items WHERE merge_key = '' AND recipe_id > 0
  ) added GROUP BY user_id, recipe_id
) lines
WHERE r.user_id = lines.user_id AND r.id = lines.recipe_id AND r.dismissed = 0 AND lines.latest > r.cooked_at;
