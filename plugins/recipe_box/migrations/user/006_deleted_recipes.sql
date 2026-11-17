-- A recipe the reader deleted is gone from every shelf, cooked or not. The row
-- stays behind as a marker, and that is deliberate: the mail that shared the
-- recipe still names it, and a mail naming a recipe is a recipe mail rather
-- than a note, so dropping the row would have turned every deleted recipe's
-- mail into a note on the Notes shelf. The marker also keeps the page from
-- coming back by itself when it is read again. Adding its link by hand is the
-- reader changing their mind, and brings it back. Its picture is dropped with
-- it, since that is most of what a recipe stores.
ALTER TABLE plugin_recipe_box_recipes ADD COLUMN IF NOT EXISTS deleted_at BIGINT NOT NULL DEFAULT 0;
