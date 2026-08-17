// File overview: Loading contact detail rows for a whole listing at once. The
// per-contact path in contacts.go issues five queries for one contact, which is
// right for a single read and wrong for the vCard export, where it multiplies
// into tens of thousands.

package store

import (
	"context"
	"database/sql"
)

// contactDetailChunk bounds one IN list. SQLite's default variable limit is 999
// and the query spends one on the user id, so this leaves generous headroom
// while still collapsing a listing into a handful of round trips.
const contactDetailChunk = 400

// loadContactDetailsForAll fills in the child rows of every contact in the
// slice. It replaces one set of queries per contact with one set per chunk,
// which is what keeps an export of a large address book from turning into tens
// of thousands of round trips.
func (s *Store) loadContactDetailsForAll(ctx context.Context, userID int64, contacts []Contact) error {
	if len(contacts) == 0 {
		return nil
	}
	byID := make(map[int64]*Contact, len(contacts))
	ids := make([]int64, 0, len(contacts))
	for i := range contacts {
		byID[contacts[i].ID] = &contacts[i]
		ids = append(ids, contacts[i].ID)
	}
	for start := 0; start < len(ids); start += contactDetailChunk {
		end := min(start+contactDetailChunk, len(ids))
		chunk := ids[start:end]
		for _, load := range []func(context.Context, int64, []int64, map[int64]*Contact) error{
			s.appendContactEmails,
			s.appendContactPhones,
			s.appendContactAddresses,
			s.appendContactURLs,
			s.appendContactIcons,
		} {
			if err := load(ctx, userID, chunk, byID); err != nil {
				return err
			}
		}
	}
	return nil
}

// queryContactDetails runs one detail query over a chunk of contact ids. The
// ordering mirrors the per-contact queries -- primary first, then id -- with
// contact_id leading so rows arrive grouped and each contact's slice keeps the
// order a single-contact read would have produced.
func (s *Store) queryContactDetails(ctx context.Context, userID int64, ids []int64, columns, table string, scan func(*sql.Rows) error) error {
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for _, id := range ids {
		args = append(args, id)
	}
	query := `SELECT ` + columns + ` FROM ` + table +
		` WHERE user_id = ? AND contact_id IN (` + sqlPlaceholders(len(ids)) + `)` +
		` ORDER BY contact_id, is_primary DESC, id`
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) appendContactEmails(ctx context.Context, userID int64, ids []int64, byID map[int64]*Contact) error {
	return s.queryContactDetails(ctx, userID, ids,
		`id, user_id, contact_id, label, email, normalized_email, is_primary, created_at, updated_at`,
		`contact_emails`, func(rows *sql.Rows) error {
			var item ContactEmail
			var created, updated int64
			var primary int
			if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.Email, &item.NormalizedEmail, &primary, &created, &updated); err != nil {
				return err
			}
			item.IsPrimary = primary != 0
			item.CreatedAt = unixTime(created)
			item.UpdatedAt = unixTime(updated)
			if contact := byID[item.ContactID]; contact != nil {
				contact.Emails = append(contact.Emails, item)
			}
			return nil
		})
}

func (s *Store) appendContactPhones(ctx context.Context, userID int64, ids []int64, byID map[int64]*Contact) error {
	return s.queryContactDetails(ctx, userID, ids,
		`id, user_id, contact_id, label, number, is_primary, created_at, updated_at`,
		`contact_phones`, func(rows *sql.Rows) error {
			var item ContactPhone
			var created, updated int64
			var primary int
			if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.Number, &primary, &created, &updated); err != nil {
				return err
			}
			item.IsPrimary = primary != 0
			item.CreatedAt = unixTime(created)
			item.UpdatedAt = unixTime(updated)
			if contact := byID[item.ContactID]; contact != nil {
				contact.Phones = append(contact.Phones, item)
			}
			return nil
		})
}

func (s *Store) appendContactAddresses(ctx context.Context, userID int64, ids []int64, byID map[int64]*Contact) error {
	return s.queryContactDetails(ctx, userID, ids,
		`id, user_id, contact_id, label, street, locality, region, postal_code, country, is_primary, created_at, updated_at`,
		`contact_addresses`, func(rows *sql.Rows) error {
			var item ContactAddress
			var created, updated int64
			var primary int
			if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.Street, &item.Locality, &item.Region, &item.PostalCode, &item.Country, &primary, &created, &updated); err != nil {
				return err
			}
			item.IsPrimary = primary != 0
			item.CreatedAt = unixTime(created)
			item.UpdatedAt = unixTime(updated)
			if contact := byID[item.ContactID]; contact != nil {
				contact.Addresses = append(contact.Addresses, item)
			}
			return nil
		})
}

func (s *Store) appendContactURLs(ctx context.Context, userID int64, ids []int64, byID map[int64]*Contact) error {
	return s.queryContactDetails(ctx, userID, ids,
		`id, user_id, contact_id, label, url, is_primary, created_at, updated_at`,
		`contact_urls`, func(rows *sql.Rows) error {
			var item ContactURL
			var created, updated int64
			var primary int
			if err := rows.Scan(&item.ID, &item.UserID, &item.ContactID, &item.Label, &item.URL, &primary, &created, &updated); err != nil {
				return err
			}
			item.IsPrimary = primary != 0
			item.CreatedAt = unixTime(created)
			item.UpdatedAt = unixTime(updated)
			if contact := byID[item.ContactID]; contact != nil {
				contact.URLs = append(contact.URLs, item)
			}
			return nil
		})
}

// appendContactIcons is separate because contact_icons has no is_primary column
// and needs the blob path joined in, so it cannot use the shared query shape.
func (s *Store) appendContactIcons(ctx context.Context, userID int64, ids []int64, byID map[int64]*Contact) error {
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx,
		`SELECT ci.id, ci.user_id, ci.contact_id, ci.blob_id, ci.content_type, ci.filename, ci.size, b.path, ci.created_at, ci.updated_at
			FROM contact_icons ci
			JOIN blobs b ON b.user_id = ci.user_id AND b.id = ci.blob_id
			WHERE ci.user_id = ? AND ci.contact_id IN (`+sqlPlaceholders(len(ids))+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var icon ContactIcon
		var created, updated int64
		if err := rows.Scan(&icon.ID, &icon.UserID, &icon.ContactID, &icon.BlobID, &icon.ContentType,
			&icon.Filename, &icon.Size, &icon.BlobPath, &created, &updated); err != nil {
			return err
		}
		icon.CreatedAt = unixTime(created)
		icon.UpdatedAt = unixTime(updated)
		if contact := byID[icon.ContactID]; contact != nil {
			stored := icon
			contact.Icon = &stored
		}
	}
	return rows.Err()
}
