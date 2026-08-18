// File overview: Contact book, me-contact, contact email, identity, and icon persistence.

package store

import (
	"context"
	"database/sql"
	"log"
	"net/mail"
	"sort"
	"strings"
	"time"
)

type contactAutocompleteCandidate struct {
	item      ContactAutocomplete
	saved     bool
	frequency int
	recency   int
}

// NormalizeContactEmail canonicalizes email addresses for contact matching and Me identity lookup.
func NormalizeContactEmail(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if addr, err := mail.ParseAddress(value); err == nil {
		value = addr.Address
	}
	return strings.ToLower(strings.Trim(value, "<> \t\r\n"))
}

// CreateContact inserts a user-owned contact plus its child email/phone/address/URL rows.
func (s *Store) CreateContact(ctx context.Context, userID int64, c Contact) (Contact, error) {
	c = normalizeContactForSave(userID, c)
	ts := nowUnix()
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return Contact{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO contacts
			(user_id, name_prefix, given_name, additional_name, family_name, name_suffix, display_name, nickname, organization, department, job_title, birthday, notes, categories, is_me, is_primary, source, google_connection_id, external_id, etag, remote_updated_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, c.NamePrefix, c.GivenName, c.AdditionalName, c.FamilyName, c.NameSuffix, c.DisplayName, c.Nickname, c.Organization, c.Department, c.JobTitle, c.Birthday, c.Notes, c.Categories, boolInt(c.IsMe), boolInt(c.IsPrimary), c.Source, c.GoogleConnectionID, c.ExternalID, c.ETag, timeUnix(c.RemoteUpdatedAt), ts, ts)
	if err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	if err := replaceContactChildren(ctx, tx, userID, id, c, ts); err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	if c.IsMe && c.IsPrimary {
		if _, err := tx.ExecContext(ctx, `UPDATE contacts SET is_primary = 0, updated_at = ? WHERE user_id = ? AND id <> ? AND is_me = 1`, ts, userID, id); err != nil {
			_ = tx.Rollback()
			return Contact{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Contact{}, err
	}
	if err := s.ensurePrimaryMeContact(ctx, userID); err != nil {
		return Contact{}, err
	}
	return s.GetContactForUser(ctx, userID, id)
}

// UpdateContact replaces a user-owned contact and synchronizes child detail rows.
func (s *Store) UpdateContact(ctx context.Context, userID, id int64, c Contact) (Contact, error) {
	return s.updateContact(ctx, userID, id, c, nil)
}

// UpdateContactAsGoogleMirror replaces a contact's data and records the Google
// person it mirrors in one transaction. The sync needs the pair to be atomic:
// data written first and a link that then loses to the unique index over the
// resource name would leave a contact rewritten with Google's fields but still
// local, which the next delta would never revisit.
func (s *Store) UpdateContactAsGoogleMirror(ctx context.Context, userID, id int64, c Contact, link ContactGoogleLink) (Contact, error) {
	if link.ConnectionID <= 0 || strings.TrimSpace(link.ExternalID) == "" {
		return Contact{}, ErrNotFound
	}
	return s.updateContact(ctx, userID, id, c, &link)
}

func (s *Store) updateContact(ctx context.Context, userID, id int64, c Contact, link *ContactGoogleLink) (Contact, error) {
	c = normalizeContactForSave(userID, c)
	ts := nowUnix()
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return Contact{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE contacts SET
			name_prefix = ?, given_name = ?, additional_name = ?, family_name = ?, name_suffix = ?, display_name = ?, nickname = ?, organization = ?, department = ?, job_title = ?, birthday = ?, notes = ?, categories = ?, is_me = ?, is_primary = ?, updated_at = ?
		WHERE user_id = ? AND id = ?`,
		c.NamePrefix, c.GivenName, c.AdditionalName, c.FamilyName, c.NameSuffix, c.DisplayName, c.Nickname, c.Organization, c.Department, c.JobTitle, c.Birthday, c.Notes, c.Categories, boolInt(c.IsMe), boolInt(c.IsPrimary), ts, userID, id)
	if err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	if n == 0 {
		_ = tx.Rollback()
		return Contact{}, ErrNotFound
	}
	if err := replaceContactChildren(ctx, tx, userID, id, c, ts); err != nil {
		_ = tx.Rollback()
		return Contact{}, err
	}
	if link != nil {
		if err := setContactGoogleLinkTx(ctx, tx, userID, id, *link, ts); err != nil {
			_ = tx.Rollback()
			return Contact{}, err
		}
	}
	if c.IsMe && c.IsPrimary {
		if _, err := tx.ExecContext(ctx, `UPDATE contacts SET is_primary = 0, updated_at = ? WHERE user_id = ? AND id <> ? AND is_me = 1`, ts, userID, id); err != nil {
			_ = tx.Rollback()
			return Contact{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Contact{}, err
	}
	if err := s.ensurePrimaryMeContact(ctx, userID); err != nil {
		return Contact{}, err
	}
	return s.GetContactForUser(ctx, userID, id)
}

// DeleteContactForUser removes one contact and dependent detail rows inside the user database.
func (s *Store) DeleteContactForUser(ctx context.Context, userID, id int64) error {
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM contacts WHERE user_id = ? AND id = ?`, userID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return s.ensurePrimaryMeContact(ctx, userID)
}

// GetContactForUser loads a contact and its detail rows for one user.
func (s *Store) GetContactForUser(ctx context.Context, userID, id int64) (Contact, error) {
	row := s.mustDataDB(ctx, userID).QueryRowContext(ctx, contactSelectSQL()+` WHERE user_id = ? AND id = ?`, userID, id)
	c, err := scanContact(row)
	if err != nil {
		return Contact{}, err
	}
	if err := s.loadContactDetails(ctx, userID, &c); err != nil {
		return Contact{}, err
	}
	return c, nil
}

// contactEmailOwnerOrder ranks the contact_emails rows that carry one address.
// An address book may hold several since user-037 -- Google lets two people
// carry the same one -- so every lookup by address has to name which of them it
// means, or the answer changes with SQLite's row order and a Me contact, a
// merge target or a reply identity silently moves to a different person.
//
// The contact whose primary address this is comes first, then the oldest row:
// the address book's own idea of whose address it is, and failing that the
// contact that has been answering to it the longest.
//
// The user's own contact outranks all of that. An address on the Me card is the
// identity outgoing mail hangs off, so when a lookup has to name one contact for
// it, naming somebody else's card -- a shared team mailbox Google also lists,
// say -- would put their name and picture on the reader's own address.
//
// It takes qualifier prefixes -- "c." and "e.", trailing dot included -- because
// every query applying it joins contacts to contact_emails, and both carry an
// `id` and a `user_id`. One ordering expression built here rather than one each
// caller re-spells is what keeps every lookup by address answering with the same
// contact.
func contactEmailOwnerOrder(contactPrefix, emailPrefix string) string {
	return ` ORDER BY ` + contactPrefix + `is_me DESC, ` +
		emailPrefix + `is_primary DESC, ` + emailPrefix + `contact_id ASC, ` + emailPrefix + `id ASC`
}

// ContactEmailHolder is one contact that carries an address, carrying just
// enough to choose between the holders: whether Google owns the row, and
// whether it is one of the user's own identities.
//
// Choosers get this rather than whole contacts because choosing never needs the
// detail rows. Loading them for every candidate would mean five queries per
// rejected holder, and the sync rejects one for every mirrored person whose
// address is already spoken for.
type ContactEmailHolder struct {
	ContactID          int64
	IsMe               bool
	Source             string
	GoogleConnectionID int64
}

// IsGoogleContact reports whether Google owns this holder, with the same rule
// the full contact uses.
func (h ContactEmailHolder) IsGoogleContact() bool {
	return h.Source == ContactSourceGoogle && h.GoogleConnectionID > 0
}

// ListContactEmailHoldersForUser returns every contact carrying one address, in
// owner order. It is the one place that order is applied: everything that
// resolves an address to a single contact reads the first holder, and everything
// that has to pick between them -- the sync adopting a local contact, an import
// choosing a merge target -- walks the list.
func (s *Store) ListContactEmailHoldersForUser(ctx context.Context, userID int64, email string) ([]ContactEmailHolder, error) {
	normalized := NormalizeContactEmail(email)
	if userID <= 0 || normalized == "" {
		return nil, nil
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT c.id, c.is_me, c.source, c.google_connection_id
		FROM contact_emails e
		JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		WHERE e.user_id = ? AND e.normalized_email = ?`+contactEmailOwnerOrder("c.", "e."), userID, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var holders []ContactEmailHolder
	seen := map[int64]bool{}
	for rows.Next() {
		var holder ContactEmailHolder
		var isMe int
		if err := rows.Scan(&holder.ContactID, &isMe, &holder.Source, &holder.GoogleConnectionID); err != nil {
			return nil, err
		}
		// One contact listing the address twice is one holder. Saving dedupes
		// by normalized address, but a database written before that did could
		// still carry the pair.
		if seen[holder.ContactID] {
			continue
		}
		seen[holder.ContactID] = true
		holder.IsMe = isMe != 0
		holders = append(holders, holder)
	}
	return holders, rows.Err()
}

// FirstLocalHolder returns the holder a local write may target: the first one no
// Google connection owns, and otherwise the first in owner order. It reports
// false when nobody holds the address at all.
//
// A local write to a Google-owned row never reaches Google, so the next sync
// reverts it -- which is why naming a sender, or merging an import, looks past a
// mirror when a local contact carries the same address. The sync's own
// findLocalMatch deliberately ranks differently and stays separate.
func FirstLocalHolder(holders []ContactEmailHolder) (ContactEmailHolder, bool) {
	if len(holders) == 0 {
		return ContactEmailHolder{}, false
	}
	for _, holder := range holders {
		if !holder.IsGoogleContact() {
			return holder, true
		}
	}
	return holders[0], true
}

// GetContactByEmailForUser finds a contact by normalized email inside one user's
// address book. Where several contacts share the address it returns the holder
// owner order names first, so a caller that only wants "the" contact for an
// address gets the same one every time.
func (s *Store) GetContactByEmailForUser(ctx context.Context, userID int64, email string) (Contact, error) {
	holders, err := s.ListContactEmailHoldersForUser(ctx, userID, email)
	if err != nil {
		return Contact{}, err
	}
	if len(holders) == 0 {
		return Contact{}, ErrNotFound
	}
	return s.GetContactForUser(ctx, userID, holders[0].ContactID)
}

// EnsureMeContactForEmail creates or updates the Me contact used for compose identities and reply targeting.
func (s *Store) EnsureMeContactForEmail(ctx context.Context, userID int64, email, displayName string) (Contact, error) {
	email = strings.TrimSpace(email)
	if NormalizeContactEmail(email) == "" {
		return Contact{}, ErrNotFound
	}
	displayName = trimLimit(displayName, 240)
	if displayName == "" {
		displayName = email
	}
	hasMe, err := s.hasMeContact(ctx, userID)
	if err != nil {
		return Contact{}, err
	}
	// The reader's own card is never a contact Google owns. Marking a mirror IsMe
	// is a local write to a Google-sourced row that nothing pushes back, so the
	// next sync reverts what it can and leaves the flag -- and every address on
	// that other person's card becomes one of the reader's From identities.
	//
	// Holders arrive with any existing Me contact first, so this takes the
	// reader's own card when they have one and otherwise the first local holder.
	// When only mirrors hold the address none is adopted; the reader gets a card
	// of their own beside them, which is only storable because user-037 lets two
	// contacts carry one address.
	holders, err := s.ListContactEmailHoldersForUser(ctx, userID, email)
	if err != nil {
		return Contact{}, err
	}
	adopt := int64(0)
	for _, holder := range holders {
		if holder.IsGoogleContact() {
			continue
		}
		adopt = holder.ContactID
		break
	}
	if adopt > 0 {
		contact, err := s.GetContactForUser(ctx, userID, adopt)
		if err != nil {
			return Contact{}, err
		}
		contact.IsMe = true
		if !hasMe {
			contact.IsPrimary = true
		}
		if strings.TrimSpace(contact.DisplayName) == "" || strings.EqualFold(strings.TrimSpace(contact.DisplayName), strings.TrimSpace(email)) {
			contact.DisplayName = displayName
		}
		updated, err := s.UpdateContact(ctx, userID, contact.ID, contact)
		if err != nil {
			return Contact{}, err
		}
		if err := s.SyncMailIdentitiesForMeContacts(ctx, userID); err != nil {
			return Contact{}, err
		}
		return updated, nil
	}
	created, err := s.CreateContact(ctx, userID, Contact{
		DisplayName: displayName,
		IsMe:        true,
		IsPrimary:   !hasMe,
		Emails: []ContactEmail{{
			Label:     "email",
			Email:     email,
			IsPrimary: true,
		}},
	})
	if err != nil {
		return Contact{}, err
	}
	if err := s.SyncMailIdentitiesForMeContacts(ctx, userID); err != nil {
		return Contact{}, err
	}
	return created, nil
}

func (s *Store) hasMeContact(ctx context.Context, userID int64) (bool, error) {
	var n int
	if err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT count(*) FROM contacts WHERE user_id = ? AND is_me = 1`, userID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ContactListFilter narrows an address-book listing.
//
// Source belongs here rather than in the caller because the listing is capped:
// filtering after the fact would drop contacts the cap already excluded, and an
// account with more contacts than the cap would appear to hold far fewer than
// the settings page reports for it.
type ContactListFilter struct {
	Query string
	// Source restricts to ContactSourceLocal or ContactSourceGoogle. Empty
	// means both.
	Source string
	// GoogleConnectionID restricts to contacts owned by one connected account.
	// It is only meaningful together with a Google source.
	GoogleConnectionID int64
	// Limit caps the answer. Zero means the default page; any positive value is
	// honoured as asked, because the caller knows what it is doing with the
	// result -- the vCard export wants the whole address book, and rounding
	// that down to a page would silently write an incomplete backup.
	Limit int
}

// defaultContactListLimit is the page an unspecified listing gets.
const defaultContactListLimit = 200

// ListContactsForUser returns contacts matching the optional address-book query.
func (s *Store) ListContactsForUser(ctx context.Context, userID int64, filter ContactListFilter) ([]Contact, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultContactListLimit
	}
	where := []string{"user_id = ?"}
	args := []any{userID}
	if query := strings.TrimSpace(filter.Query); query != "" {
		like := "%" + strings.ToLower(query) + "%"
		where = append(where, `(
			lower(display_name) LIKE ? OR lower(given_name) LIKE ? OR lower(family_name) LIKE ? OR lower(organization) LIKE ?
			OR EXISTS (SELECT 1 FROM contact_emails WHERE contact_emails.user_id = contacts.user_id AND contact_emails.contact_id = contacts.id AND lower(email) LIKE ?)
		)`)
		args = append(args, like, like, like, like, like)
	}
	switch strings.TrimSpace(filter.Source) {
	case ContactSourceGoogle:
		// A contact whose connection is gone is local again, which is what the
		// zero connection id in the row means.
		where = append(where, "source = ? AND google_connection_id > 0")
		args = append(args, ContactSourceGoogle)
		if filter.GoogleConnectionID > 0 {
			where = append(where, "google_connection_id = ?")
			args = append(args, filter.GoogleConnectionID)
		}
	case ContactSourceLocal:
		where = append(where, "(source <> ? OR google_connection_id = 0)")
		args = append(args, ContactSourceGoogle)
	}
	args = append(args, limit)
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, contactSelectSQL()+`
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY lower(CASE WHEN display_name <> '' THEN display_name ELSE family_name || ' ' || given_name END), id
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contacts, err := scanContacts(rows)
	if err != nil {
		return nil, err
	}
	if err := s.loadContactDetailsForAll(ctx, userID, contacts); err != nil {
		return nil, err
	}
	return contacts, nil
}

// AutocompleteContactsForUser merges saved contacts with recent user-owned
// correspondents. Recent addresses remain suggestions only; they are not imported.
func (s *Store) AutocompleteContactsForUser(ctx context.Context, userID int64, query string, limit int) ([]ContactAutocomplete, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	query = strings.ToLower(strings.TrimSpace(query))
	like := "%" + query + "%"
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT
			c.id, c.display_name, c.given_name, c.family_name, e.email, e.label,
			CASE WHEN ci.id IS NULL THEN '' ELSE '/contacts/' || c.id || '/icon' END AS icon_url
		FROM contact_emails e
		JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		LEFT JOIN contact_icons ci ON ci.user_id = c.user_id AND ci.contact_id = c.id
		WHERE e.user_id = ? AND (? = '' OR lower(e.email) LIKE ? OR lower(c.display_name) LIKE ? OR lower(c.given_name) LIKE ? OR lower(c.family_name) LIKE ? OR lower(c.organization) LIKE ?)
		ORDER BY e.is_primary DESC, lower(CASE WHEN c.display_name <> '' THEN c.display_name ELSE c.given_name || ' ' || c.family_name END), lower(e.email)
			LIMIT ?`, userID, query, like, like, like, like, like, limit*2)
	if err != nil {
		return nil, err
	}
	candidates := map[string]*contactAutocompleteCandidate{}
	for rows.Next() {
		var item ContactAutocomplete
		var display, given, family string
		if err := rows.Scan(&item.ContactID, &display, &given, &family, &item.Email, &item.Label, &item.IconURL); err != nil {
			rows.Close()
			return nil, err
		}
		item.Name = contactName(display, given, family)
		key := NormalizeContactEmail(item.Email)
		if key != "" {
			candidates[key] = &contactAutocompleteCandidate{item: item, saved: true}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	own, err := s.autocompleteOwnAddresses(ctx, userID)
	if err != nil {
		return nil, err
	}
	recentRows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT from_addr, to_addr, cc_addr
		FROM messages WHERE user_id = ? ORDER BY date_unix DESC, id DESC LIMIT 250`, userID)
	if err != nil {
		return nil, err
	}
	recentIndex := 250
	for recentRows.Next() {
		var from, to, cc string
		if err := recentRows.Scan(&from, &to, &cc); err != nil {
			recentRows.Close()
			return nil, err
		}
		for _, address := range autocompleteAddresses(from, to, cc) {
			key := NormalizeContactEmail(address.Address)
			if key == "" || own[key] || !autocompleteAddressMatches(address, query) {
				continue
			}
			entry := candidates[key]
			if entry == nil {
				entry = &contactAutocompleteCandidate{item: ContactAutocomplete{
					Name:  strings.TrimSpace(address.Name),
					Email: address.Address,
					Label: "Recent",
				}}
				candidates[key] = entry
			}
			entry.frequency++
			if recentIndex > entry.recency {
				entry.recency = recentIndex
			}
		}
		recentIndex--
	}
	if err := recentRows.Err(); err != nil {
		recentRows.Close()
		return nil, err
	}
	recentRows.Close()

	ranked := make([]*contactAutocompleteCandidate, 0, len(candidates))
	for _, item := range candidates {
		ranked = append(ranked, item)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left := autocompleteCandidateScore(ranked[i], query)
		right := autocompleteCandidateScore(ranked[j], query)
		if left != right {
			return left > right
		}
		return strings.ToLower(ranked[i].item.Email) < strings.ToLower(ranked[j].item.Email)
	})
	out := make([]ContactAutocomplete, 0, min(limit, len(ranked)))
	for _, item := range ranked {
		out = append(out, item.item)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *Store) autocompleteOwnAddresses(ctx context.Context, userID int64) (map[string]bool, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT email FROM mail_accounts WHERE user_id = ?
		UNION SELECT e.email FROM contact_emails e JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		WHERE e.user_id = ? AND c.is_me = 1`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return nil, err
		}
		if normalized := NormalizeContactEmail(address); normalized != "" {
			out[normalized] = true
		}
	}
	return out, rows.Err()
}

func autocompleteAddresses(values ...string) []*mail.Address {
	var out []*mail.Address
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > 8192 {
			value = value[:8192]
		}
		addresses, err := mail.ParseAddressList(value)
		if err != nil {
			addresses = nil
			for _, part := range strings.Split(value, ";") {
				if address, parseErr := mail.ParseAddress(strings.TrimSpace(part)); parseErr == nil {
					addresses = append(addresses, address)
				}
			}
		}
		for _, address := range addresses {
			key := NormalizeContactEmail(address.Address)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, address)
		}
	}
	return out
}

func autocompleteAddressMatches(address *mail.Address, query string) bool {
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(address.Address), query) ||
		strings.Contains(strings.ToLower(address.Name), query)
}

func autocompleteCandidateScore(item *contactAutocompleteCandidate, query string) int {
	score := item.recency + min(item.frequency, 20)*20
	if item.saved {
		score += 500
	}
	if query == "" {
		return score
	}
	email := strings.ToLower(item.item.Email)
	name := strings.ToLower(item.item.Name)
	switch {
	case email == query:
		score += 2000
	case strings.HasPrefix(email, query):
		score += 1200
	case strings.HasPrefix(name, query):
		score += 1000
	default:
		score += 400
	}
	return score
}

// ListContactEmailsForSearchBoostForUser returns non-Me contact email addresses
// used as optional from: ranking nudges during best-match search. The caller
// applies the actual boost weight, so this helper stays cacheable and tenant-scoped.
func (s *Store) ListContactEmailsForSearchBoostForUser(ctx context.Context, userID int64, limit int) ([]string, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT DISTINCT e.email
		FROM contact_emails e
		JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		WHERE e.user_id = ? AND c.is_me = 0 AND e.normalized_email != ''
		ORDER BY lower(e.email)
		LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, limit)
	seen := map[string]bool{}
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		identity := SenderIdentity(email)
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		out = append(out, identity)
	}
	return out, rows.Err()
}

// ListMeContactsForUser returns contacts marked as the signed-in user's own identities.
func (s *Store) ListMeContactsForUser(ctx context.Context, userID int64) ([]Contact, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, contactSelectSQL()+`
		WHERE user_id = ? AND is_me = 1
		ORDER BY is_primary DESC, lower(CASE WHEN display_name <> '' THEN display_name ELSE given_name || ' ' || family_name END), id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contacts, err := scanContacts(rows)
	if err != nil {
		return nil, err
	}
	if err := s.loadContactDetailsForAll(ctx, userID, contacts); err != nil {
		return nil, err
	}
	return contacts, nil
}

// ListMeContactEmailsForUser returns normalized Me email addresses for reply and sender detection.
func (s *Store) ListMeContactEmailsForUser(ctx context.Context, userID int64) ([]string, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT e.email
		FROM contact_emails e
		JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		WHERE e.user_id = ? AND c.is_me = 1
		ORDER BY c.is_primary DESC, e.is_primary DESC, e.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		if strings.TrimSpace(email) != "" {
			out = append(out, email)
		}
	}
	return out, rows.Err()
}

// SetContactIcon records a contact icon blob for one user-owned contact.
func (s *Store) SetContactIcon(ctx context.Context, userID, contactID, blobID int64, contentType, filename string, size int64) (ContactIcon, error) {
	if _, err := s.GetContactForUser(ctx, userID, contactID); err != nil {
		return ContactIcon{}, err
	}
	blob, err := s.GetBlobForUser(ctx, userID, blobID)
	if err != nil {
		return ContactIcon{}, err
	}
	// The row this upsert is about to overwrite owns a blob nothing else will
	// reference afterwards. Contact sync re-imports a photo every time it
	// changes at Google, so without this the replaced files accumulate for the
	// life of the account.
	replaced := s.currentContactIconBlobID(ctx, userID, contactID)
	ts := nowUnix()
	_, err = s.mustDataDB(ctx, userID).ExecContext(ctx, `INSERT INTO contact_icons (user_id, contact_id, blob_id, content_type, filename, size, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, contact_id) DO UPDATE SET blob_id = excluded.blob_id, content_type = excluded.content_type, filename = excluded.filename, size = excluded.size, updated_at = excluded.updated_at`,
		userID, contactID, blob.ID, strings.TrimSpace(contentType), strings.TrimSpace(filename), size, ts, ts)
	if err != nil {
		return ContactIcon{}, err
	}
	s.releaseReplacedContactIcon(ctx, userID, replaced, blob.ID)
	return s.GetContactIconForUser(ctx, userID, contactID)
}

// currentContactIconBlobID reads the blob a contact's icon points at, or 0 when
// it has none. A read failure is reported as "no previous icon": the caller
// uses this only to release a superseded blob, and skipping that leaks a file
// rather than losing one still in use.
func (s *Store) currentContactIconBlobID(ctx context.Context, userID, contactID int64) int64 {
	var blobID int64
	if err := s.mustDataDB(ctx, userID).QueryRowContext(ctx,
		`SELECT blob_id FROM contact_icons WHERE user_id = ? AND contact_id = ?`,
		userID, contactID).Scan(&blobID); err != nil {
		return 0
	}
	return blobID
}

// releaseReplacedContactIcon queues the superseded blob for cleanup. It is
// best-effort by design: the queue only accepts a blob nothing references, and
// a failure here costs disk rather than correctness, so it must not fail the
// icon write that already succeeded.
func (s *Store) releaseReplacedContactIcon(ctx context.Context, userID, replaced, current int64) {
	if replaced <= 0 || replaced == current {
		return
	}
	if _, _, err := s.QueueBlobCleanupIfUnreferenced(ctx, userID, replaced); err != nil {
		log.Printf("queue replaced contact icon blob user_id=%d blob_id=%d: %v", userID, replaced, err)
	}
}

// GetContactIconForUser loads icon metadata for one user-owned contact.
func (s *Store) GetContactIconForUser(ctx context.Context, userID, contactID int64) (ContactIcon, error) {
	var icon ContactIcon
	var created, updated int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT ci.id, ci.user_id, ci.contact_id, ci.blob_id, ci.content_type, ci.filename, ci.size, b.path, ci.created_at, ci.updated_at
		FROM contact_icons ci
		JOIN blobs b ON b.user_id = ci.user_id AND b.id = ci.blob_id
		WHERE ci.user_id = ? AND ci.contact_id = ?`, userID, contactID).
		Scan(&icon.ID, &icon.UserID, &icon.ContactID, &icon.BlobID, &icon.ContentType, &icon.Filename, &icon.Size, &icon.BlobPath, &created, &updated)
	icon.CreatedAt = unixTime(created)
	icon.UpdatedAt = unixTime(updated)
	return icon, err
}

// GetContactIconByEmailForUser loads icon metadata by matching a contact email.
// Several contacts may hold the address, so the picture is the one belonging to
// the holder owner order names first -- the same contact the name beside it
// comes from. Without the order the avatar would be whichever row SQLite
// scanned first and could show one of the two people while the name shows the
// other.
func (s *Store) GetContactIconByEmailForUser(ctx context.Context, userID int64, email string) (ContactIcon, error) {
	normalized := NormalizeContactEmail(email)
	if normalized == "" {
		return ContactIcon{}, ErrNotFound
	}
	var contactID int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT e.contact_id
		FROM contact_emails e
		JOIN contact_icons ci ON ci.user_id = e.user_id AND ci.contact_id = e.contact_id
		JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
		WHERE e.user_id = ? AND e.normalized_email = ?`+contactEmailOwnerOrder("c.", "e.")+`
		LIMIT 1`, userID, normalized).Scan(&contactID)
	if err != nil {
		return ContactIcon{}, err
	}
	return s.GetContactIconForUser(ctx, userID, contactID)
}

// ListContactIconsByEmailsForUser loads contact icon metadata for a batch of
// normalized emails. The first row seen for an address wins, so the query
// carries the same owner order the single lookup uses: a shared address would
// otherwise be answered by one holder in the mail list and the other in the
// open thread, and differently again on the next page load.
func (s *Store) ListContactIconsByEmailsForUser(ctx context.Context, userID int64, emails []string) (map[string]ContactIcon, error) {
	normalized := make([]string, 0, len(emails))
	seen := map[string]bool{}
	for _, email := range emails {
		key := NormalizeContactEmail(email)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		normalized = append(normalized, key)
	}
	out := map[string]ContactIcon{}
	if userID <= 0 || len(normalized) == 0 {
		return out, nil
	}
	var hasIcon int
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT 1 FROM contact_icons WHERE user_id = ? LIMIT 1`, userID).Scan(&hasIcon)
	if IsNotFound(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(normalized); start += 500 {
		end := start + 500
		if end > len(normalized) {
			end = len(normalized)
		}
		chunk := normalized[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		for _, email := range chunk {
			args = append(args, email)
		}
		rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT e.normalized_email, ci.id, ci.user_id, ci.contact_id, ci.blob_id, ci.content_type, ci.filename, ci.size, b.path, ci.created_at, ci.updated_at
			FROM contact_emails e
			JOIN contact_icons ci ON ci.user_id = e.user_id AND ci.contact_id = e.contact_id
			JOIN contacts c ON c.user_id = e.user_id AND c.id = e.contact_id
			JOIN blobs b ON b.user_id = ci.user_id AND b.id = ci.blob_id
			WHERE e.user_id = ? AND e.normalized_email IN (`+sqlPlaceholders(len(chunk))+`)`+
			contactEmailOwnerOrder("c.", "e."), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key string
			var icon ContactIcon
			var created, updated int64
			if err := rows.Scan(&key, &icon.ID, &icon.UserID, &icon.ContactID, &icon.BlobID, &icon.ContentType, &icon.Filename, &icon.Size, &icon.BlobPath, &created, &updated); err != nil {
				_ = rows.Close()
				return nil, err
			}
			icon.CreatedAt = unixTime(created)
			icon.UpdatedAt = unixTime(updated)
			if _, exists := out[key]; !exists {
				out[key] = icon
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteContactIconForUser removes icon metadata from a user-owned contact.
func (s *Store) DeleteContactIconForUser(ctx context.Context, userID, contactID int64) error {
	removed := s.currentContactIconBlobID(ctx, userID, contactID)
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM contact_icons WHERE user_id = ? AND contact_id = ?`, userID, contactID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	s.releaseReplacedContactIcon(ctx, userID, removed, 0)
	return nil
}

func contactSelectSQL() string {
	return `SELECT id, user_id, name_prefix, given_name, additional_name, family_name, name_suffix, display_name, nickname, organization, department, job_title, birthday, notes, categories, is_me, is_primary, source, google_connection_id, external_id, etag, remote_updated_at, created_at, updated_at FROM contacts`
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanContact(row rowScanner) (Contact, error) {
	var c Contact
	var created, updated, remoteUpdated int64
	var isMe, isPrimary int
	err := row.Scan(&c.ID, &c.UserID, &c.NamePrefix, &c.GivenName, &c.AdditionalName, &c.FamilyName, &c.NameSuffix, &c.DisplayName, &c.Nickname, &c.Organization, &c.Department, &c.JobTitle, &c.Birthday, &c.Notes, &c.Categories, &isMe, &isPrimary, &c.Source, &c.GoogleConnectionID, &c.ExternalID, &c.ETag, &remoteUpdated, &created, &updated)
	c.IsMe = isMe != 0
	c.IsPrimary = isPrimary != 0
	c.RemoteUpdatedAt = unixTime(remoteUpdated)
	c.CreatedAt = unixTime(created)
	c.UpdatedAt = unixTime(updated)
	return c, err
}

func scanContacts(rows *sql.Rows) ([]Contact, error) {
	var out []Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) loadContactDetails(ctx context.Context, userID int64, c *Contact) error {
	emails, err := s.listContactEmails(ctx, userID, c.ID)
	if err != nil {
		return err
	}
	phones, err := s.listContactPhones(ctx, userID, c.ID)
	if err != nil {
		return err
	}
	addresses, err := s.listContactAddresses(ctx, userID, c.ID)
	if err != nil {
		return err
	}
	urls, err := s.listContactURLs(ctx, userID, c.ID)
	if err != nil {
		return err
	}
	c.Emails = emails
	c.Phones = phones
	c.Addresses = addresses
	c.URLs = urls
	if icon, err := s.GetContactIconForUser(ctx, userID, c.ID); err == nil {
		c.Icon = &icon
	} else if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// replaceContactEmails writes a contact's addresses without giving them new ids.
//
// Addresses are the one detail list something else points at: an outgoing
// identity references a contact_emails row and cascades on its deletion. The
// delete-then-insert the other lists use therefore destroyed the identity for
// an address the edit never touched -- with its signature, its SMTP server and
// its folder choices -- and only looked harmless while identities were derived
// from the address book and silently rebuilt. Rows are matched by normalized
// address and updated in place, so an id outlives every edit that keeps the
// address, and only an address the user really removed takes its identity with
// it.
func replaceContactEmails(ctx context.Context, tx *sql.Tx, userID, contactID int64, emails []ContactEmail, ts int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, normalized_email FROM contact_emails WHERE user_id = ? AND contact_id = ? ORDER BY id`, userID, contactID)
	if err != nil {
		return err
	}
	stored := map[string]int64{}
	var storedIDs []int64
	for rows.Next() {
		var id int64
		var normalized string
		if err := rows.Scan(&id, &normalized); err != nil {
			rows.Close()
			return err
		}
		storedIDs = append(storedIDs, id)
		if normalized != "" {
			if _, seen := stored[normalized]; !seen {
				stored[normalized] = id
			}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	needsPrimary := !anyPrimary(emails, func(item ContactEmail) bool { return item.IsPrimary })
	kept := map[int64]bool{}
	for _, email := range emails {
		email.Email = strings.TrimSpace(email.Email)
		email.NormalizedEmail = NormalizeContactEmail(email.Email)
		if email.Email == "" || email.NormalizedEmail == "" {
			continue
		}
		primary := boolInt(email.IsPrimary || needsPrimary)
		needsPrimary = false
		label := strings.TrimSpace(email.Label)
		if id, ok := stored[email.NormalizedEmail]; ok && !kept[id] {
			if _, err := tx.ExecContext(ctx, `UPDATE contact_emails SET label = ?, email = ?, normalized_email = ?, is_primary = ?, updated_at = ?
				WHERE user_id = ? AND id = ?`, label, email.Email, email.NormalizedEmail, primary, ts, userID, id); err != nil {
				return err
			}
			kept[id] = true
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_emails (user_id, contact_id, label, email, normalized_email, is_primary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, userID, contactID, label, email.Email, email.NormalizedEmail, primary, ts, ts); err != nil {
			return err
		}
	}
	for _, id := range storedIDs {
		if kept[id] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM contact_emails WHERE user_id = ? AND id = ?`, userID, id); err != nil {
			return err
		}
	}
	return nil
}

// replaceContactChildren rewrites a contact's detail rows.
//
// The primary flag is stored as given, with one exception: a list where nothing
// was marked promotes the first row that actually gets stored. Promoting by
// index instead would mark row zero even when a later row is the marked one,
// which is how a merged contact ended up with two primary emails.
func replaceContactChildren(ctx context.Context, tx *sql.Tx, userID, contactID int64, c Contact, ts int64) error {
	if err := replaceContactEmails(ctx, tx, userID, contactID, c.Emails, ts); err != nil {
		return err
	}
	for _, table := range []string{"contact_phones", "contact_addresses", "contact_urls"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id = ? AND contact_id = ?`, userID, contactID); err != nil {
			return err
		}
	}
	phoneNeedsPrimary := !anyPrimary(c.Phones, func(item ContactPhone) bool { return item.IsPrimary })
	for _, phone := range c.Phones {
		phone.Number = strings.TrimSpace(phone.Number)
		if phone.Number == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_phones (user_id, contact_id, label, number, is_primary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, contactID, strings.TrimSpace(phone.Label), phone.Number, boolInt(phone.IsPrimary || phoneNeedsPrimary), ts, ts); err != nil {
			return err
		}
		phoneNeedsPrimary = false
	}
	addressNeedsPrimary := !anyPrimary(c.Addresses, func(item ContactAddress) bool { return item.IsPrimary })
	for _, addr := range c.Addresses {
		if strings.TrimSpace(addr.Street+addr.Locality+addr.Region+addr.PostalCode+addr.Country) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_addresses (user_id, contact_id, label, street, locality, region, postal_code, country, is_primary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, userID, contactID, strings.TrimSpace(addr.Label), strings.TrimSpace(addr.Street), strings.TrimSpace(addr.Locality), strings.TrimSpace(addr.Region), strings.TrimSpace(addr.PostalCode), strings.TrimSpace(addr.Country), boolInt(addr.IsPrimary || addressNeedsPrimary), ts, ts); err != nil {
			return err
		}
		addressNeedsPrimary = false
	}
	urlNeedsPrimary := !anyPrimary(c.URLs, func(item ContactURL) bool { return item.IsPrimary })
	for _, u := range c.URLs {
		u.URL = strings.TrimSpace(u.URL)
		if u.URL == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO contact_urls (user_id, contact_id, label, url, is_primary, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, contactID, strings.TrimSpace(u.Label), u.URL, boolInt(u.IsPrimary || urlNeedsPrimary), ts, ts); err != nil {
			return err
		}
		urlNeedsPrimary = false
	}
	return nil
}

// anyPrimary reports whether a detail list already names one entry primary.
func anyPrimary[T any](items []T, primary func(T) bool) bool {
	for _, item := range items {
		if primary(item) {
			return true
		}
	}
	return false
}

func (s *Store) listContactEmails(ctx context.Context, userID, contactID int64) ([]ContactEmail, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, contact_id, label, email, normalized_email, is_primary, created_at, updated_at
		FROM contact_emails WHERE user_id = ? AND contact_id = ? ORDER BY is_primary DESC, id`, userID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactEmail
	for rows.Next() {
		var item ContactEmail
		var created, updated int64
		var primary int
		if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.Email, &item.NormalizedEmail, &primary, &created, &updated); err != nil {
			return nil, err
		}
		item.IsPrimary = primary != 0
		item.CreatedAt = unixTime(created)
		item.UpdatedAt = unixTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) listContactPhones(ctx context.Context, userID, contactID int64) ([]ContactPhone, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, contact_id, label, number, is_primary, created_at, updated_at
		FROM contact_phones WHERE user_id = ? AND contact_id = ? ORDER BY is_primary DESC, id`, userID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactPhone
	for rows.Next() {
		var item ContactPhone
		var created, updated int64
		var primary int
		if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.Number, &primary, &created, &updated); err != nil {
			return nil, err
		}
		item.IsPrimary = primary != 0
		item.CreatedAt = unixTime(created)
		item.UpdatedAt = unixTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) listContactAddresses(ctx context.Context, userID, contactID int64) ([]ContactAddress, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, contact_id, label, street, locality, region, postal_code, country, is_primary, created_at, updated_at
		FROM contact_addresses WHERE user_id = ? AND contact_id = ? ORDER BY is_primary DESC, id`, userID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactAddress
	for rows.Next() {
		var item ContactAddress
		var created, updated int64
		var primary int
		if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.Street, &item.Locality, &item.Region, &item.PostalCode, &item.Country, &primary, &created, &updated); err != nil {
			return nil, err
		}
		item.IsPrimary = primary != 0
		item.CreatedAt = unixTime(created)
		item.UpdatedAt = unixTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) listContactURLs(ctx context.Context, userID, contactID int64) ([]ContactURL, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, contact_id, label, url, is_primary, created_at, updated_at
		FROM contact_urls WHERE user_id = ? AND contact_id = ? ORDER BY is_primary DESC, id`, userID, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContactURL
	for rows.Next() {
		var item ContactURL
		var created, updated int64
		var primary int
		if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.URL, &primary, &created, &updated); err != nil {
			return nil, err
		}
		item.IsPrimary = primary != 0
		item.CreatedAt = unixTime(created)
		item.UpdatedAt = unixTime(updated)
		out = append(out, item)
	}
	return out, rows.Err()
}

func normalizeContactForSave(userID int64, c Contact) Contact {
	c.UserID = userID
	c.NamePrefix = trimLimit(c.NamePrefix, 80)
	c.GivenName = trimLimit(c.GivenName, 160)
	c.AdditionalName = trimLimit(c.AdditionalName, 160)
	c.FamilyName = trimLimit(c.FamilyName, 160)
	c.NameSuffix = trimLimit(c.NameSuffix, 80)
	c.DisplayName = trimLimit(c.DisplayName, 240)
	c.Nickname = trimLimit(c.Nickname, 160)
	c.Organization = trimLimit(c.Organization, 240)
	c.Department = trimLimit(c.Department, 160)
	c.JobTitle = trimLimit(c.JobTitle, 160)
	c.Birthday = trimLimit(c.Birthday, 32)
	c.Notes = trimLimit(c.Notes, 12000)
	c.Categories = trimLimit(c.Categories, 1000)
	if c.DisplayName == "" {
		c.DisplayName = contactName("", c.GivenName, c.FamilyName)
	}
	if c.DisplayName == "" && len(c.Emails) > 0 {
		c.DisplayName = strings.TrimSpace(c.Emails[0].Email)
	}
	if c.DisplayName == "" {
		c.DisplayName = strings.TrimSpace(c.Organization)
	}
	// Exactly one primary per detail list. Merging two versions of a person --
	// a vCard import, a Google sync -- appends entries that were each primary
	// on their own side, and replaceContactChildren stores every one of them as
	// primary. Two primaries is a state nothing downstream is prepared for.
	// The unique index user-037 dropped was also the only thing stopping one
	// contact from carrying an address twice -- two labels, two spellings of the
	// same address. Everything that resolves an address to a row -- the identity
	// it sends from, the reply target, the picture beside a sender -- then has
	// two answers where the question has one. Google is allowed to hold the
	// pair; Rolltop stores it once.
	c.Emails = dedupeContactEmails(c.Emails)
	keepSinglePrimary(c.Emails, func(item *ContactEmail) *bool { return &item.IsPrimary })
	keepSinglePrimary(c.Phones, func(item *ContactPhone) *bool { return &item.IsPrimary })
	keepSinglePrimary(c.Addresses, func(item *ContactAddress) *bool { return &item.IsPrimary })
	keepSinglePrimary(c.URLs, func(item *ContactURL) *bool { return &item.IsPrimary })
	c.Source, c.GoogleConnectionID, c.ExternalID, c.ETag = normalizeContactProvenance(
		c.Source, c.GoogleConnectionID, c.ExternalID, c.ETag)
	if c.Source == ContactSourceLocal {
		c.RemoteUpdatedAt = time.Time{}
	}
	return c
}

// dedupeContactEmails keeps one entry per normalized address, the first, and
// gives it the primary flag if any of its copies carried one. Entries that
// normalize to nothing are left alone: replaceContactChildren drops them, and
// collapsing them here would merge two unrelated malformed addresses.
func dedupeContactEmails(emails []ContactEmail) []ContactEmail {
	if len(emails) < 2 {
		return emails
	}
	out := make([]ContactEmail, 0, len(emails))
	index := map[string]int{}
	for _, email := range emails {
		key := NormalizeContactEmail(email.Email)
		if key == "" {
			out = append(out, email)
			continue
		}
		if at, ok := index[key]; ok {
			if email.IsPrimary {
				out[at].IsPrimary = true
			}
			continue
		}
		index[key] = len(out)
		out = append(out, email)
	}
	return out
}

// keepSinglePrimary leaves the first entry marked primary and clears the rest,
// promoting the first when none was marked. That last part matches what
// replaceContactChildren writes anyway, so the stored rows and the struct the
// caller handed in no longer disagree about which entry is the primary one.
func keepSinglePrimary[T any](items []T, primary func(*T) *bool) {
	found := false
	for i := range items {
		flag := primary(&items[i])
		if *flag && !found {
			found = true
			continue
		}
		*flag = false
	}
	if !found && len(items) > 0 {
		*primary(&items[0]) = true
	}
}

func contactName(display, given, family string) string {
	display = strings.TrimSpace(display)
	if display != "" {
		return display
	}
	return strings.TrimSpace(strings.Join(strings.Fields(strings.TrimSpace(given)+" "+strings.TrimSpace(family)), " "))
}

func trimLimit(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return value
}

func (s *Store) ensurePrimaryMeContact(ctx context.Context, userID int64) error {
	var n int
	if err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT count(*) FROM contacts WHERE user_id = ? AND is_me = 1 AND is_primary = 1`, userID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `UPDATE contacts SET is_primary = 1, updated_at = ? WHERE id = (
		SELECT id FROM contacts WHERE user_id = ? AND is_me = 1 ORDER BY id LIMIT 1
	)`, nowUnix(), userID)
	return err
}
