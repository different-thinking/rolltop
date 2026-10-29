# Recipe box plugin

Turns the recipe links a reader mails themselves into a cookbook: one card per
recipe with its picture, ingredients, steps, time and servings, under
**Organizer → Recipes** (`/apps/recipes`). The mails those links came in are
filed out of the Inbox into a recipe folder.

## Structure

- `manifest.json` declares the runtime backend binary and frontend bundle.
- `recipe/` is the pure part: link extraction from a mail, the guarded page and
  picture fetch, and reading a page's schema.org `Recipe` (JSON-LD, then
  microdata) with an OpenGraph preview as the fallback.
- `backend/` is the Go plugin: settings, the scan (backfill), the fetch queue,
  filing mail, and the API under `/api/plugins/recipe_box/`.
- `frontend/` is the cookbook view, the settings page and their styles.
- `migrations/user/` holds the plugin's tenant-scoped tables.

## What it reads

Only mail **one of the reader's own addresses sent**, and by default only when
every recipient is one of their own addresses too (a note-to-self; a mail with
no visible recipient reached them by Bcc and counts). Own addresses are the
connected accounts and the sending identities. Newsletters and mail from anybody
else are never read, whatever they link to. Sent, Drafts, Trash, Junk and
Gmail's system folders are never read either: the Sent copy is the reader's own
record of writing the note, and moving it would empty their Sent folder.

Nothing happens for a reader who has not switched the box on in
**Settings → Plugins → Recipes**: the absence of a settings row means off.

## How a recipe is recognised

A link is a recipe when its page **says so** in the structured data search
engines read: a schema.org `Recipe` in JSON-LD (standalone, in a list, in
`@graph`, or as a `mainEntity`) or in microdata. That is the whole test. It is
the page's own statement rather than a guess from the site name or the words
on it: nearly every recipe site publishes it because Google builds its recipe
cards from it, and nothing that is not a recipe has a reason to.

Everything else a page offers is kept as a preview. A link whose page publishes
no recipe (a video, a social post, an article) shows up under **Not recognised**
and can be filed as a recipe by hand; its mail is filed with it. A page can also
be added directly with **Add link**.

## The three steps

The work is one background loop per process:

1. **Scan.** Walk the reader's mail by row id from a cursor and record the
   links of every note-to-self. Starting the cursor at zero is the backfill;
   new mail is the same walk continuing. The stored-message hook only wakes the
   loop, so a sync turn never waits on a recipe site.
2. **Fetch.** Ask each recorded link's page what it is. The dialer refuses
   private and special-purpose addresses on every redirect hop
   (`remoteimages.SafeDialContext`), sends no cookies or referrer, and bounds
   every read in time and bytes. Requests to one site are spaced out. A passing
   failure is retried on a backoff; a 404 is final. The recipe's picture is
   stored with it (raster formats only, sniffed from the bytes), so the card
   shows without asking the site again.
3. **File.** Move every mail that carried a recipe into the recipe folder of
   its own account (default `Recipes`, configurable), creating the folder on the
   server on first use. A folder the box created is taken out of All Mail at
   once; a folder that already existed keeps the reader's settings, and the
   settings page offers to take it out. A mail is filed **once**: dragging it
   back is the reader's decision and the box does not refile it.

A mail is followed by its Message-ID, because a move deletes the row and the
destination's sync stores it again under a new id; the recipe's "Shared in"
list always links to where the mail is now.
