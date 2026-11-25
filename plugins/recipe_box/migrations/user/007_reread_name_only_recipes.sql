-- A page whose Recipe block names the dish but carries neither ingredients nor
-- steps used to be read as no recipe at all, and filed as an article or a
-- plain link -- hr-fernsehen.de prints every recipe that way, with the lists
-- only in the article's text. Such a page is now completed from its own
-- "Zutaten" and "Zubereitung" lists. What the page declared was not stored, so
-- the pages it could have been are read once more: the links answered as an
-- article or a plain link. The loop fetches them at its usual pace, and a page
-- that is still no recipe is filed exactly as before.
UPDATE plugin_recipe_box_links SET status = 'pending', attempts = 0, next_attempt_at = 0, error = ''
WHERE status = 'page' AND forced = 0 AND kind IN ('article', 'link');
