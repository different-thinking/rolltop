package autocrypt

import (
	"bytes"
	"context"
	"net/mail"
	"net/textproto"
	"strings"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
	"rolltop/plugins/client_side_pgp/backend/keystore"
)

func OutboundMailHeaders(ctx context.Context, db *store.Store, userID int64, identity plugins.MailIdentityContext) ([]plugins.MailHeader, error) {
	if identity.Preferences["autocrypt_enabled"] != "true" || identity.ID == 0 {
		return nil, nil
	}
	key, err := keystore.ActiveIdentityPublicKeyForUser(ctx, db, userID, identity.ID)
	if err != nil || strings.TrimSpace(key.PublicKeyArmored) == "" {
		return nil, nil
	}
	keyData, ok := KeyDataFromArmoredPublicKey(key.PublicKeyArmored)
	if !ok {
		return nil, nil
	}
	value := HeaderValue(identity.Email, keyData)
	if value == "" {
		return nil, nil
	}
	return []plugins.MailHeader{{Name: "Autocrypt", Value: value}}, nil
}

// ImportIncomingMessage learns a contact's public key from the Autocrypt header
// of a message they sent. The header is unauthenticated, so this follows the
// Autocrypt peer-state rules that keep a spoofed message from planting or
// swapping a key: the message must have a single sender, carry exactly one
// Autocrypt header, and that header's addr must match the sender. Junk is
// excluded upstream (the host does not run this hook for spam folders), and the
// keystore refuses to let a discovered key displace one the user set by hand.
func ImportIncomingMessage(ctx context.Context, db *store.Store, userID int64, raw []byte, parsedFrom string) error {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	// Autocrypt applies only to a message with exactly one sender. A From that
	// lists several addresses is out of scope and could be used to smuggle a key
	// under an address the recipient never actually corresponded with.
	if addrs, err := mail.ParseAddressList(msg.Header.Get("From")); err == nil && len(addrs) != 1 {
		return nil
	}
	sender := store.NormalizeContactEmail(parsedFrom)
	if sender == "" {
		sender = store.NormalizeContactEmail(msg.Header.Get("From"))
	}
	if sender == "" {
		return nil
	}
	// Exactly one Autocrypt header is honoured. Zero means nothing to learn; two
	// or more is malformed or hostile, and the spec says such a message must be
	// treated as if it carried none.
	headers := ParseHeaderValues(textproto.MIMEHeader(msg.Header).Values("Autocrypt"))
	if len(headers) != 1 {
		return nil
	}
	header := headers[0]
	if !strings.EqualFold(store.NormalizeContactEmail(header.Addr), sender) {
		return nil
	}
	return keystore.SaveAutocryptContactKey(ctx, db, userID, keystore.ContactPublicKeyInput{
		Email:            header.Addr,
		Label:            header.Addr,
		PublicKeyArmored: header.PublicKey,
		SourceKind:       "autocrypt",
		SourceDetail:     header.Addr,
	})
}
