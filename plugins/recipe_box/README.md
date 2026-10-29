# Recipe box plugin

Turns the recipe links a reader mails themselves into a cookbook: one card per
recipe with its picture, ingredients, steps, time and servings, under
**Organizer → Recipes** (`/apps/recipes`). The mails those links came in are
filed out of the Inbox into a recipe folder. Everything else the reader mailed
themselves is a note, sorted onto shelves under **Organizer → Notes**
(`/apps/notes`).

## Structure

- `manifest.json` declares the runtime backend binary and frontend bundle.
- `recipe/` is the pure part: link extraction from a mail, the guarded page and
  picture fetch, and reading a page's schema.org `Recipe` (JSON-LD, then
  microdata) with an OpenGraph preview as the fallback, and what kind of page
  anything else is (`kind.go`).
- `backend/` is the Go plugin: settings, the scan (backfill), the fetch queue,
  filing mail, the notes overview, and the API under `/api/plugins/recipe_box/`.
- `frontend/` is the cookbook view, the notes view, the settings page and their
  styles.
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
no recipe (a video, a social post, an article) becomes a note, and can be filed
as a recipe by hand from there; its mail is filed with it. A page can also be
added directly with **Add link**.

A recipe is **to cook** until the reader marks it cooked on its page; cooked
recipes have a tab of their own and keep the date (`cooked_at`). Marking one
does not count as an edit and does not reorder the cookbook.

## Notes

A note is a recorded note-to-self that carries no recipe: a link that turned
out to be something else, or a few lines of text with no link at all (a mail
with only an attachment and no words is not one). It is the **mail**, not the
link: the reader's own text beside the links belongs to all of them, and is
kept (`mails.body`, bounded) so the overview can show it after blob retention
has pruned the raw message.

Its shelf is the kind of its first link, and the kind is what the page says
about itself, never a guess from words (`recipe.Classify`): the schema.org
types it publishes (`Product`, `Event`, `VideoObject`, `Place`, `Book`,
`MusicRecording`, `SoftwareApplication`, `NewsArticle` …) first, then its
`og:type`, then the platform it lives on (YouTube is video, Spotify is music,
GitHub is tech, Google Maps is a place), and otherwise just a link. A product
keeps its price and an event its start as the note's detail. Google documents,
Drive files and map links are not fetched at all: they answer a server with a
sign-in page, so their host is all that can be known.

A note is open until the reader marks it done; done notes keep their own tab.
Removing a note hides it from the overview and keeps it from being filed.

Filing notes out of the Inbox is a **separate switch, off by default**, into a
folder of its own (`Notes`): the box was switched on by readers who agreed to
have their recipe mails moved, not their other mail. A note is filed only once
every page it links to has answered, so a mail is never put on the notes shelf
and then turns out to have been a recipe.

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
   back is the reader's decision and the box does not refile it. Notes go the
   same way into their own folder when the reader asked for that.

A mail is followed by its Message-ID, because a move deletes the row and the
destination's sync stores it again under a new id; the recipe's "Shared in"
list always links to where the mail is now. A mail without a Message-ID is
followed by its content hash inside its account, so it is still filed only once.

The scan never passes a row younger than a minute (`scanSettle`): ids are
handed out on insert and become visible on commit, so a cursor that ran ahead
of a still-open sync transaction would skip the rows it was writing. While a
backfill keeps the loop busy, rounds revisit only the readers with work left;
everyone else is asked again on new mail and once per idle interval.
