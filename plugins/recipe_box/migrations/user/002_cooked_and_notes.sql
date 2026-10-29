-- A recipe the reader has cooked is one they marked so, and the cookbook keeps
-- the two apart: what is still to try, and what has been made.
ALTER TABLE plugin_recipe_box_recipes ADD COLUMN IF NOT EXISTS cooked_at BIGINT NOT NULL DEFAULT 0;

-- What a shared link that is not a recipe points at, for the notes overview,
-- and the one detail its card shows (a price, the date of an event).
ALTER TABLE plugin_recipe_box_links ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
ALTER TABLE plugin_recipe_box_links ADD COLUMN IF NOT EXISTS detail TEXT NOT NULL DEFAULT '';

-- A note is a note-to-self that carried no recipe. It keeps what the reader
-- wrote, line breaks and all, and whether they are done with it or took it out.
ALTER TABLE plugin_recipe_box_mails ADD COLUMN IF NOT EXISTS body TEXT NOT NULL DEFAULT '';
ALTER TABLE plugin_recipe_box_mails ADD COLUMN IF NOT EXISTS done_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE plugin_recipe_box_mails ADD COLUMN IF NOT EXISTS note_dismissed INTEGER NOT NULL DEFAULT 0;

-- Filing notes out of the Inbox is off for everybody until they switch it on:
-- an upgrade must not start moving every note a reader ever wrote themselves.
ALTER TABLE plugin_recipe_box_settings ADD COLUMN IF NOT EXISTS notes_move_enabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plugin_recipe_box_settings ADD COLUMN IF NOT EXISTS notes_folder_name TEXT NOT NULL DEFAULT 'Notes';

CREATE INDEX IF NOT EXISTS idx_plugin_recipe_box_mails_notes ON plugin_recipe_box_mails(user_id, note_dismissed, done_at, date_unix DESC);

-- Pages read before kinds existed are read once more to learn theirs, and the
-- mailbox is walked again for the notes without a link the first walk passed
-- over. Neither moves mail that was filed already: a mail is filed once.
UPDATE plugin_recipe_box_links SET status = 'pending', attempts = 0, next_attempt_at = 0 WHERE status = 'page';
UPDATE plugin_recipe_box_links SET kind = 'recipe' WHERE status = 'recipe';
UPDATE plugin_recipe_box_settings SET scan_cursor = 0, scan_finished_at = 0
