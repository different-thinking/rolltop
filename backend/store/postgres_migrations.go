// File overview: Incremental schema migrations layered on the frozen baseline.
// This is the upgrade path §WP7 of docs/postgres-migration-plan.md left open:
// the baseline's checksum stays the recorded identity of a database's origin,
// and every schema change after the first durable database is a numbered entry
// here. The model is plugin_migrations — one checksummed row per migration,
// applied under the schema lock only when something is outstanding — with the
// rows kept in schema_migrations under the existing "postgres" scope so a
// database created before this mechanism needs nothing retrofitted.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// postgresMigration is one schema change layered on the baseline. Shipped
// entries are immutable: the checksum recorded at apply time is re-verified on
// every start, and editing an applied entry is refused the same way an edited
// baseline is.
type postgresMigration struct {
	// Version orders and identifies the migration ("0001-message-search").
	// Zero-padded so the lexicographic order in the database matches the list.
	Version string
	// Statements run in order inside one implicit transaction, together with
	// the INSERT that records the row — all or nothing, like the baseline.
	Statements []string
}

// postgresMigrations is append-only. The list order is the apply order, and a
// database's applied rows must always be a prefix of it: this binary refuses
// rows it does not know (a newer binary wrote them) and gaps it cannot explain.
var postgresMigrations = []postgresMigration{
	{
		// The full-text search rows for the Postgres search backend
		// (docs/search-postgres-plan.md §3). Deliberately narrow: volatile
		// flags and the mailbox stay in messages and are joined at query time,
		// and deleting a message deletes its search row through the cascade.
		Version: "0001-message-search",
		Statements: []string{
			`CREATE TABLE message_search (
				message_id bigint PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
				user_id bigint NOT NULL,
				tsv tsvector NOT NULL
			)`,
			`CREATE INDEX idx_message_search_tsv ON message_search USING GIN (tsv)`,
			`CREATE INDEX idx_message_search_user ON message_search (user_id)`,
		},
	},
	{
		// The fuzzy-match word list beside the vector: the distinct normalized
		// words of the indexed text, probed with pg_trgm word similarity. Only
		// the column is a migration — the extension and its trigram index are
		// runtime-optional (EnsureTrigramSearch), because a hoster may not
		// allow CREATE EXTENSION and search must degrade to exact matching
		// then, not refuse to start.
		Version: "0002-message-search-words",
		Statements: []string{
			`ALTER TABLE message_search ADD COLUMN words text NOT NULL DEFAULT ''`,
		},
	},
	{
		// Gmail submission moves from implicit TLS on 465 to STARTTLS on 587
		// (gmailSMTPPort, backend/web/api_account.go). The endpoint was written
		// by the server rather than typed, so an account saved before this
		// change carries a port its owner never chose, and on a network that
		// blocks 465 it fails with a connection that times out. Only rows that
		// still carry the written endpoint are moved: a host somebody entered
		// themselves is theirs. The move is not a one-way door -- 465 is
		// offered in the settings for the network where it is 587 that is
		// blocked -- so a row this puts on the wrong port can be put back.
		//
		// Both tables hold the endpoint. smtp_accounts is the outgoing server,
		// and it is the row every send builds its envelope from.
		// mail_accounts.smtp_* is what an incoming account was saved with, and
		// it is copied into a new outgoing server when a user who has none
		// saves a mailbox (ensureMailAccountOnboarding, api_account.go).
		// Moving only the first would let the old port arrive in smtp_accounts
		// afterwards, through a row this migration can no longer reach.
		Version: "0003-gmail-submission-port",
		Statements: []string{
			`UPDATE smtp_accounts SET port = 587
				WHERE auth_type = 'google_oauth' AND host = 'smtp.gmail.com' AND port = 465`,
			`UPDATE mail_accounts SET smtp_port = 587
				WHERE auth_type = 'google_oauth' AND smtp_host = 'smtp.gmail.com' AND smtp_port = 465`,
		},
	},
	{
		// Mail this Rolltop sent, recognised when the provider's own copy of it
		// comes back through sync. Keeping Sent out of the whole-account lists
		// is a property of the *folder* (show_in_all_mail), and Gmail's All Mail
		// is one folder holding both what arrived and what the user sent -- so a
		// reader who mirrors it saw every reply and every filter forward turn up
		// in Inbox and in Relevant, as mail waiting on them.
		//
		// The ledger is what makes the copy recognisable: a Message-ID this
		// installation generated, remembered for the account it was sent from.
		// It is deliberately not "mail from my own address" -- a printer, an
		// alarm system or a spoof can send as the reader, and hiding that would
		// hide real mail.
		//
		// The rows are kept rather than expired. A folder whose UIDVALIDITY the
		// server resets is re-imported from scratch, years after the fact, and
		// every one of those arrivals has to reach the same conclusion as the
		// first import did -- a window that had closed by then would flood the
		// lists with mail the reader sent. They go with their account and with
		// their user, which is what the two cascades are for.
		Version: "0004-own-outgoing-copies",
		Statements: []string{
			`CREATE TABLE outgoing_message_ids (
				user_id bigint NOT NULL,
				account_id bigint NOT NULL,
				message_id_header text COLLATE "C" NOT NULL,
				created_at bigint NOT NULL,
				PRIMARY KEY (user_id, account_id, message_id_header)
			)`,
			`ALTER TABLE outgoing_message_ids ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE`,
			`ALTER TABLE outgoing_message_ids ADD FOREIGN KEY (account_id) REFERENCES mail_accounts(id) ON DELETE CASCADE`,
			`ALTER TABLE messages ADD COLUMN own_outgoing_copy bigint NOT NULL DEFAULT 0`,
		},
	},
	{
		// Index the foreign keys that point at messages(id) and blobs(id). The
		// baseline declares these references but no index led by the referencing
		// column, so PostgreSQL's referential-integrity check for every ON DELETE
		// CASCADE / SET NULL ran as a sequential (or wrong-order index) scan of
		// the child table once per deleted parent row. Emptying a Trash folder of
		// tens of thousands of messages therefore went quadratic — each deleted
		// message re-scanning attachments, locations and the rest — and every
		// DELETE FROM blobs (RESTRICT) and blob reference recheck scanned all of
		// the tenant's messages/attachments. The existing composite indexes lead
		// with user_id (or other columns), which the RI check, keyed on the
		// foreign-key column alone, cannot use. IF NOT EXISTS so an operator who
		// added one by hand is not an error. CREATE INDEX (not CONCURRENTLY)
		// because a migration runs inside a transaction; that is acceptable here
		// because migrations run only after this process has acquired the
		// exclusive instance lock (OpenPostgres takes it before ensurePostgresSchema,
		// and a rolling deploy waits for the previous server to release it), so
		// there is no concurrent writer for the index build's lock to block. The
		// only cost is a one-time startup delay while the indexes build on a large
		// existing database, before the server begins serving.
		//
		// Numbered 0005 because 0004-own-outgoing-copies landed on main first, and
		// the migration list is append-only: a database that already applied 0004
		// must find the applied versions as a prefix of this list.
		Version: "0005-fk-indexes",
		Statements: []string{
			`CREATE INDEX IF NOT EXISTS idx_fk_attachments_message_id ON attachments (message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_attachments_blob_id ON attachments (blob_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_messages_blob_id ON messages (blob_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_contact_icons_blob_id ON contact_icons (blob_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_locations_message_id ON locations (message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_message_snoozes_message_id ON message_snoozes (message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_new_mail_events_message_id ON new_mail_events (message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_pending_inbox_arrivals_message_id ON pending_inbox_arrivals (message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_plugin_language_messages_message_id ON plugin_language_messages (message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_snooze_reminder_events_message_id ON snooze_reminder_events (message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_one_click_unsubscribe_message_id ON plugin_one_click_unsubscribe_sends (message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_pending_move_notifications_consumed_message_id ON pending_move_notifications (consumed_message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_expunged_fingerprints_consumed_message_id ON expunged_message_fingerprints (consumed_message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_message_transfers_source_message_id ON message_transfers (source_message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_message_transfers_consumed_message_id ON message_transfers (consumed_message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_spam_classifications_user_message_id ON plugin_experimental_spam_classifications (user_id, message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_fk_spam_feedback_user_message_id ON plugin_experimental_spam_feedback (user_id, message_id)`,
		},
	},
	{
		// Enforce at the database what GetOrCreateMailboxFromDiscovery and
		// UpdateMailboxSettings only checked read-then-write: within one account a
		// mail role (inbox, sent, drafts, trash, junk, all) names at most one
		// folder. Two callers racing that check could both see the role as free
		// and assign it to different folders, after which the UI picked one
		// arbitrarily. A partial unique index makes the losing write fail instead.
		// Roles are stored as '' when unassigned and any number of folders may be
		// unassigned, so the index covers only rows that actually carry a role.
		//
		// An existing database may already hold a raced duplicate, which would make
		// a plain unique index fail to build. The migration first demotes the extra
		// rows in each duplicate group to '' -- keeping the lowest id, i.e. the
		// folder discovered first -- so the index can be created. Like every
		// migration this runs under the exclusive instance lock (see 0005-fk-indexes),
		// so no concurrent writer can reintroduce a duplicate between the cleanup
		// and the index build.
		Version: "0006-mailbox-role-unique",
		Statements: []string{
			`UPDATE mailboxes AS m SET role = ''
				FROM (
					SELECT id, ROW_NUMBER() OVER (
						PARTITION BY user_id, account_id, role ORDER BY id
					) AS rn
					FROM mailboxes
					WHERE role <> ''
				) dup
				WHERE m.id = dup.id AND dup.rn > 1`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_mailboxes_account_role_unique
				ON mailboxes (user_id, account_id, role) WHERE role <> ''`,
		},
	},
	{
		// Partial indexes for the three global retention/maintenance queries in
		// store/message_body.go. Those run across every tenant at once (s.db, no
		// user_id filter), and every existing messages index leads with user_id,
		// so each maintenance pass fell back to a sequential scan of the whole
		// messages table — the cost growing with the whole installation regardless
		// of how little was actually due. Each index is partial and ordered by the
		// (date_unix|created_at, id) the matching query filters and orders on, so a
		// pass range-scans only the rows still eligible and stops at its LIMIT:
		//
		//   * idx_messages_prunable_blob_path -> ListMessagesWithPrunableBlobs
		//     (WHERE blob_path <> '' AND date_unix < ?). blob_path is non-empty
		//     only while a message still owns a local raw file, a small and
		//     shrinking set as retention prunes them.
		//   * idx_messages_compactable_html + idx_messages_compactable_text ->
		//     CompactMessageBodiesBefore, whose two disjoint eligibility arms
		//     (an HTML body to drop, or a long plaintext body to shorten) each get
		//     an index the rewritten UNION ALL query can range-scan and merge.
		//     The text arm's length threshold is the literal DefaultMessageBodyPreviewBytes
		//     (4096); the query selects against that same literal so the predicate
		//     matches. Migrations are immutable, so were that default ever changed
		//     a new migration would carry the new value — until then the query
		//     still returns correct rows, only without this index.
		//   * idx_blobs_message_cache -> ListMessagesWithExpiredCachedBlobs, which
		//     drives off blobs (WHERE kind = 'message-cache' AND created_at < ?)
		//     and joins back to messages by the FK index added in 0005.
		//
		// CREATE INDEX (not CONCURRENTLY) for the same reason as 0005-fk-indexes:
		// migrations run under the exclusive instance lock, so there is no
		// concurrent writer for the build to block. IF NOT EXISTS so an operator
		// who added an equivalent index by hand is not an error.
		Version: "0007-maintenance-partial-indexes",
		Statements: []string{
			`CREATE INDEX IF NOT EXISTS idx_messages_prunable_blob_path
				ON messages (date_unix, id) WHERE blob_path <> ''`,
			`CREATE INDEX IF NOT EXISTS idx_messages_compactable_html
				ON messages (date_unix, id) WHERE body_html <> ''`,
			`CREATE INDEX IF NOT EXISTS idx_messages_compactable_text
				ON messages (date_unix, id) WHERE body_html = '' AND length(body_text) > 4096`,
			`CREATE INDEX IF NOT EXISTS idx_blobs_message_cache
				ON blobs (created_at, id) WHERE kind = 'message-cache'`,
		},
	},
	{
		// Record whether a mirrored message carried an Autocrypt header, decided
		// once at parse time. The thread view used to fetch every message's full
		// RFC822 source (base64 attachments and all) on every open just to look for
		// this header and offer to import the sender's key; the flag lets the client
		// skip that fetch for the messages that do not have one — the large majority.
		//
		// Existing rows default to 0 (no header known). They were parsed before this
		// column existed, so the client will not offer an Autocrypt import prompt for
		// mail synced earlier — but the server-side ImportIncomingMessage hook
		// already imported those keys at the time they arrived, so the peer key is
		// in the keystore regardless. New mail sets the flag from the parse.
		Version: "0008-message-autocrypt-header",
		Statements: []string{
			`ALTER TABLE messages ADD COLUMN has_autocrypt_header bigint NOT NULL DEFAULT 0`,
		},
	},
	{
		// Which classifier generation decided a message's category
		// (store.CategoryVersion). Categories used to be a one-way door: the
		// backfill only ever looked at rows with no category at all, so mail
		// filed before a rule changed stayed where the old rule put it forever
		// — and a category added later started out empty except for mail that
		// happened to arrive after it. The column turns that into ordinary
		// background work: rows an older generation filed are re-read a batch
		// at a time, while keeping the answer they already have until the new
		// one lands, so no list goes blank while the pass runs.
		//
		// Existing rows default to 0, which is below every shipped generation:
		// the first run of a build carrying this column re-classifies the
		// mailbox once. Adding the column is metadata-only in PostgreSQL — the
		// default is not volatile — so the migration itself does not rewrite
		// the table.
		//
		// The index is not partial. A predicate naming one generation would
		// have to be replaced by a new migration on every bump, and the
		// selection reads `category_version < $1` with a bound parameter, which
		// a partial index could not be matched against anyway.
		Version: "0009-message-category-version",
		Statements: []string{
			`ALTER TABLE messages ADD COLUMN category_version bigint NOT NULL DEFAULT 0`,
			`CREATE INDEX idx_messages_category_version ON messages (user_id, category_version, id)`,
		},
	},
	{
		// Retention: how long mail is kept before it is thrown away, and how
		// long the Trash keeps what was thrown away before the server is told
		// to delete it for good.
		//
		// Two tables because the two answers are shaped differently. The Trash
		// rule is one number per reader -- it is the same rule for every
		// account, since a Trash folder is a Trash folder -- and it carries the
		// sweep bookkeeping, so a restart does not re-run a purge that has just
		// happened. The category rules are one row per category the reader has
		// an opinion about; a category with no row is one nothing is deleted
		// from, which is why the absence of a row is the off state rather than
		// a default that would start deleting mail on upgrade.
		//
		// A cutoff is expressed either relatively ("older than 30 days", which
		// moves with the calendar) or as one fixed day, and both spellings are
		// stored rather than resolved on save: a relative rule that had been
		// resolved to a date once would stop being a retention policy the day
		// after it was written.
		//
		// A relative cutoff keeps its own unit rather than being reduced to a
		// number of days, because the calendar is what the reader meant: six
		// months is six months, not a hundred and eighty days, and reducing it
		// would also make "30 days" and "1 month" the same stored rule and read
		// one of them back as the other.
		Version: "0010-retention",
		Statements: []string{
			`CREATE TABLE retention_settings (
				user_id bigint PRIMARY KEY,
				trash_enabled bigint NOT NULL DEFAULT 1,
				trash_days bigint NOT NULL DEFAULT 30,
				categories_swept_at bigint NOT NULL DEFAULT 0,
				trash_swept_at bigint NOT NULL DEFAULT 0,
				created_at bigint NOT NULL,
				updated_at bigint NOT NULL
			)`,
			`ALTER TABLE retention_settings ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE`,
			`CREATE TABLE category_retention_rules (
				user_id bigint NOT NULL,
				category text COLLATE "C" NOT NULL,
				mode text COLLATE "C" NOT NULL DEFAULT 'off' CHECK(mode IN ('off', 'relative', 'fixed')),
				cutoff_count bigint NOT NULL DEFAULT 0,
				cutoff_unit text COLLATE "C" NOT NULL DEFAULT 'days' CHECK(cutoff_unit IN ('days', 'months', 'years')),
				before_unix bigint NOT NULL DEFAULT 0,
				created_at bigint NOT NULL,
				updated_at bigint NOT NULL,
				PRIMARY KEY (user_id, category)
			)`,
			`ALTER TABLE category_retention_rules ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE`,
			// Everyone who already has an account gets the Trash rule switched
			// off, explicitly, so that upgrading cannot start deleting their
			// mail off a mail server. The shipped default for a reader created
			// after this point is the other way round -- the Trash empties
			// itself after 30 days -- and the absence of a row is what carries
			// it, exactly as the absence of a category row carries "delete
			// nothing". A row here is a decision, and this is the one decision
			// an upgrade is allowed to make on somebody's behalf: the safe one.
			`INSERT INTO retention_settings
				(user_id, trash_enabled, trash_days, categories_swept_at, trash_swept_at, created_at, updated_at)
			SELECT id, 0, 30, 0, 0,
				EXTRACT(EPOCH FROM now())::bigint, EXTRACT(EPOCH FROM now())::bigint
			FROM users
			ON CONFLICT (user_id) DO NOTHING`,
		},
	},
	{
		// Parcels: what the mail says is on its way, and which messages said it.
		//
		// A shipment is its own row rather than a column on a message because
		// the two do not line up. One parcel is talked about by four messages --
		// the shop's dispatch note, the carrier's hand-over, "it arrives today",
		// and the delivery report -- and one message can name a parcel per item
		// of a large order. The reader's question is about the parcel, so the
		// parcel is the row and the messages hang off it.
		//
		// The identity is (carrier, tracking number). That is what makes the
		// shop's mail and the carrier's mail one row, and it is why the carrier
		// may be empty: a message that labelled a number without saying whose it
		// is still describes a parcel, and an empty carrier is a distinct
		// identity rather than a match for every carrier.
		//
		// expected_date is a plain "YYYY-MM-DD" and not a timestamp. What a
		// carrier announces is a day; storing an instant would mean inventing a
		// timezone nobody stated, and C collation makes the text sort
		// chronologically anyway.
		//
		// reported_at is the date of the message the current answer came from,
		// which is what lets messages be read out of order: a later message
		// overwrites the day and the status, an earlier one does not, so a
		// backfill reading last week's mail cannot undo "delivered".
		Version: "0011-shipments",
		Statements: []string{
			`CREATE TABLE shipments (
				id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
				user_id bigint NOT NULL,
				carrier text COLLATE "C" NOT NULL DEFAULT '',
				tracking_number text COLLATE "C" NOT NULL,
				expected_date text COLLATE "C" NOT NULL DEFAULT '',
				window_start text COLLATE "C" NOT NULL DEFAULT '',
				window_end text COLLATE "C" NOT NULL DEFAULT '',
				status text COLLATE "C" NOT NULL DEFAULT 'announced' CHECK(status IN ('announced', 'out_for_delivery', 'delivered')),
				reported_at bigint NOT NULL DEFAULT 0,
				created_at bigint NOT NULL,
				updated_at bigint NOT NULL
			)`,
			`ALTER TABLE shipments ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE`,
			`CREATE UNIQUE INDEX idx_shipments_number ON shipments (user_id, carrier, tracking_number)`,
			`CREATE INDEX idx_shipments_expected ON shipments (user_id, expected_date, status)`,
			`CREATE TABLE shipment_messages (
				shipment_id bigint NOT NULL,
				message_id bigint NOT NULL,
				user_id bigint NOT NULL,
				created_at bigint NOT NULL,
				PRIMARY KEY (shipment_id, message_id)
			)`,
			`ALTER TABLE shipment_messages ADD FOREIGN KEY (shipment_id) REFERENCES shipments(id) ON DELETE CASCADE`,
			`ALTER TABLE shipment_messages ADD FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE`,
			`ALTER TABLE shipment_messages ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE`,
			`CREATE INDEX idx_shipment_messages_message ON shipment_messages (user_id, message_id)`,
			`ALTER TABLE messages ADD COLUMN delivery_version bigint NOT NULL DEFAULT 0`,
			`CREATE INDEX idx_messages_delivery_version ON messages (user_id, delivery_version, id)`,
		},
	},
	{
		// What the reader says about a parcel, which outranks what the mail said.
		//
		// The classifier can only ever report what a message stated, and two
		// states it cannot get out of that: a parcel that arrived without the
		// carrier ever writing to say so, and a number that was never a parcel.
		// Both leave a row sitting on the list with nothing that will ever move
		// it, and the reader is the only one who knows. So they get a column,
		// and it wins: the same shape as the per-sender category corrections,
		// for the same reason.
		//
		// It is deliberately not a rewrite of `status`. Keeping the reader's
		// answer beside the extracted one means a later message still updates
		// what the carrier says without stepping on the correction, and the
		// correction can be taken back.
		Version: "0012-shipment-manual-status",
		Statements: []string{
			`ALTER TABLE shipments ADD COLUMN manual_status text COLLATE "C" NOT NULL DEFAULT ''
				CHECK(manual_status IN ('', 'delivered', 'dismissed'))`,
			`ALTER TABLE shipments ADD COLUMN manual_status_at bigint NOT NULL DEFAULT 0`,
		},
	},
	{
		// Bills the mail says are owed, on the same shape as shipments above and
		// for the same reason: several messages talk about one invoice -- the
		// invoice, a reminder, a dunning letter, the payment confirmation -- and
		// the useful row is the invoice with the mail hanging off it.
		//
		// The identity is (issuer, reference) and not the number alone, which is
		// the one real difference from a parcel. A tracking number was issued by
		// exactly one carrier in the world; "2026-001" is a number half the
		// senders in a mailbox hand out every January, so the sender's own
		// domain is half the key. reference is the number where the document
		// stated one and a fallback where it did not, which is why it and not
		// `number` carries the unique index -- see mailparse.invoiceReference.
		//
		// due_date is a plain "YYYY-MM-DD" for the same reason expected_date is:
		// a payment term is a calendar day, and storing an instant would mean
		// inventing a timezone nobody stated.
		//
		// dunning_level only ever rises. A dunning letter read after the invoice
		// it chases -- which the backfill does by definition, since it reads
		// newest first -- must not be undone by the older message, and unlike
		// the status, which the newest message legitimately sets, being chased
		// is a thing that happened and stays true.
		Version: "0013-invoices",
		Statements: []string{
			`CREATE TABLE invoices (
				id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
				user_id bigint NOT NULL,
				issuer text COLLATE "C" NOT NULL,
				reference text COLLATE "C" NOT NULL,
				number text COLLATE "C" NOT NULL DEFAULT '',
				due_date text COLLATE "C" NOT NULL DEFAULT '',
				amount text COLLATE "C" NOT NULL DEFAULT '',
				currency text COLLATE "C" NOT NULL DEFAULT '',
				status text COLLATE "C" NOT NULL DEFAULT 'open' CHECK(status IN ('open', 'paid')),
				settlement text COLLATE "C" NOT NULL DEFAULT ''
					CHECK(settlement IN ('', 'transfer', 'direct_debit', 'card', 'wallet')),
				dunning_level integer NOT NULL DEFAULT 0 CHECK(dunning_level BETWEEN 0 AND 3),
				manual_status text COLLATE "C" NOT NULL DEFAULT ''
					CHECK(manual_status IN ('', 'paid', 'dismissed')),
				manual_status_at bigint NOT NULL DEFAULT 0,
				manual_due_date text COLLATE "C" NOT NULL DEFAULT '',
				reported_at bigint NOT NULL DEFAULT 0,
				created_at bigint NOT NULL,
				updated_at bigint NOT NULL
			)`,
			`ALTER TABLE invoices ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE`,
			`CREATE UNIQUE INDEX idx_invoices_reference ON invoices (user_id, issuer, reference)`,
			`CREATE INDEX idx_invoices_due ON invoices (user_id, due_date, status)`,
			`CREATE TABLE invoice_messages (
				invoice_id bigint NOT NULL,
				message_id bigint NOT NULL,
				user_id bigint NOT NULL,
				created_at bigint NOT NULL,
				PRIMARY KEY (invoice_id, message_id)
			)`,
			`ALTER TABLE invoice_messages ADD FOREIGN KEY (invoice_id) REFERENCES invoices(id) ON DELETE CASCADE`,
			`ALTER TABLE invoice_messages ADD FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE`,
			`ALTER TABLE invoice_messages ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE`,
			`CREATE INDEX idx_invoice_messages_message ON invoice_messages (user_id, message_id)`,
			`ALTER TABLE messages ADD COLUMN invoice_version bigint NOT NULL DEFAULT 0`,
			// The category is in the index because it is the backfill's whole
			// selection: only mail already filed as paperwork is ever re-read
			// for a bill, which is what makes a pass that opens attachments
			// affordable at all.
			`CREATE INDEX idx_messages_invoice_version ON messages (user_id, category, invoice_version, id)`,
		},
	},
}

func postgresMigrationChecksum(m postgresMigration) string {
	return postgresMigrationIdentity(m).current
}

// postgresMigrationIdentity is the migration's checksum under both algorithms,
// so a row an older build recorded is recognised as this same migration rather
// than read as an edit to a shipped one. See checksumRecognised for why the row
// is left as it is instead of being rewritten.
func postgresMigrationIdentity(m postgresMigration) checksumIdentity {
	return checksumIdentity{
		current: schemaChecksum(postgresSchemaScope, m.Version, m.Statements...),
		legacy:  schemaChecksumLegacy(postgresSchemaScope, m.Version, m.Statements...),
	}
}

// postgresSchemaState is what one read of schema_migrations says about this
// database, classified against the binary's baseline checksum and migration
// list. Errors carry the disagreements; the ordinary cases are the two bools
// and the outstanding suffix.
type postgresSchemaState struct {
	// BaselinePresent reports a matching baseline row. False with a nil error
	// means the empty-database case.
	BaselinePresent bool
	// Outstanding holds the migrations this database has not applied yet, in
	// apply order. Meaningful only when BaselinePresent is true; a fresh
	// database gets the whole list after its baseline.
	Outstanding []postgresMigration
}

// readPostgresSchemaState classifies the database in a single query, so the
// every-restart fast path stays one read: baseline row and migration rows come
// back together, and only a database with work to do takes the schema lock.
func readPostgresSchemaState(ctx context.Context, conn *sql.Conn, baseline checksumIdentity, migrations []postgresMigration) (postgresSchemaState, error) {
	var table sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass($1)::text`,
		schemaMigrationsQualified()).Scan(&table); err != nil {
		return postgresSchemaState{}, postgresError("inspect the schema", err)
	}
	if !table.Valid {
		return postgresSchemaState{Outstanding: migrations}, nil
	}
	rows, err := conn.QueryContext(ctx,
		`SELECT version, checksum FROM schema_migrations WHERE scope = $1`, postgresSchemaScope)
	if err != nil {
		return postgresSchemaState{}, postgresError("read the schema version", err)
	}
	defer rows.Close()
	applied := make(map[string]string)
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return postgresSchemaState{}, postgresError("read the schema version", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return postgresSchemaState{}, postgresError("read the schema version", err)
	}
	return classifyPostgresSchemaState(applied, baseline, migrations)
}

// classifyPostgresSchemaState turns the recorded rows into a decision. It is
// split from the read so the refusal cases can be tested without a database.
func classifyPostgresSchemaState(applied map[string]string, baseline checksumIdentity, migrations []postgresMigration) (postgresSchemaState, error) {
	recorded, baselinePresent := applied[postgresSchemaVersion]
	if baselinePresent && !baseline.recognises(recorded) {
		return postgresSchemaState{}, errors.New("postgres: schema baseline checksum mismatch: this database was created from a different baseline than the running binary carries, and there is no upgrade path between the two")
	}
	if !baselinePresent {
		if len(applied) > 0 {
			// Rows without a baseline is not a state any Rolltop start can
			// produce; whatever wrote them, this database is not ours to use.
			return postgresSchemaState{}, fmt.Errorf("postgres: schema_migrations carries %d row(s) but no baseline; this database was not created by Rolltop", len(applied))
		}
		return postgresSchemaState{Outstanding: migrations}, nil
	}
	known := make(map[string]bool, len(migrations)+1)
	known[postgresSchemaVersion] = true
	for _, m := range migrations {
		known[m.Version] = true
	}
	var unknown []string
	for version := range applied {
		if !known[version] {
			unknown = append(unknown, version)
		}
	}
	if len(unknown) > 0 {
		return postgresSchemaState{}, fmt.Errorf("postgres: the database has applied migration(s) this binary does not know (%s); it was written by a newer build, and running an older one against it is how data gets lost", strings.Join(sortedStrings(unknown), ", "))
	}
	firstOutstanding := -1
	for i, m := range migrations {
		checksum, isApplied := applied[m.Version]
		if !isApplied {
			if firstOutstanding < 0 {
				firstOutstanding = i
			}
			continue
		}
		if firstOutstanding >= 0 {
			return postgresSchemaState{}, fmt.Errorf("postgres: migration %s is applied but earlier migration %s is not; the migration history of this database cannot be explained", m.Version, migrations[firstOutstanding].Version)
		}
		if !postgresMigrationIdentity(m).recognises(checksum) {
			return postgresSchemaState{}, fmt.Errorf("postgres: migration %s was edited after this database applied it; shipped migrations are immutable", m.Version)
		}
	}
	state := postgresSchemaState{BaselinePresent: true}
	if firstOutstanding >= 0 {
		state.Outstanding = migrations[firstOutstanding:]
	}
	return state, nil
}

// applyPostgresMigration runs one migration as a single simple-protocol script
// whose last statement records its row — the same atomicity argument as
// applyPostgresBaseline: DDL without its row would read back as tampering on
// the next start, so both land in the same implicit transaction or not at all.
func applyPostgresMigration(ctx context.Context, conn *sql.Conn, m postgresMigration) error {
	var script strings.Builder
	for _, stmt := range m.Statements {
		script.WriteString(strings.TrimSpace(stmt))
		script.WriteString(";\n")
	}
	script.WriteString(recordMigrationStatement(m))
	if err := execPostgresScript(ctx, conn, script.String()); err != nil {
		return postgresError(fmt.Sprintf("apply schema migration %s", m.Version), err)
	}
	return nil
}

// recordMigrationStatement renders the schema_migrations row as literal SQL,
// inlined for the simple protocol exactly as recordBaselineStatement is.
func recordMigrationStatement(m postgresMigration) string {
	return fmt.Sprintf(
		`INSERT INTO schema_migrations (scope, version, applied_at, checksum) VALUES (%s, %s, %d, %s);`,
		quoteSQLLiteral(postgresSchemaScope), quoteSQLLiteral(m.Version), nowUnix(), quoteSQLLiteral(postgresMigrationChecksum(m)))
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
