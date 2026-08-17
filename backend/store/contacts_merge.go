// File overview: Merging two versions of the same person into one contact.
// vCard import and Google contact sync both hit the same question -- this
// address book already knows this person, now what -- and answering it twice
// would let the two paths drift.

package store

import "strings"

// MergeContacts folds extra into base without losing anything.
//
// base wins every scalar it has a value for; extra only fills gaps. Detail
// rows are unioned, keyed by their normalized value, so a phone number one side
// knows and the other does not survives regardless of which side it came from.
// The caller decides which version is authoritative by choosing which one is
// base: a vCard import keeps the stored contact, and a Google sync keeps
// Google's copy.
//
// Identity flags and provenance are deliberately not touched here. Whether the
// result is a Me contact, and which account owns it, is the caller's decision
// and depends on why the merge is happening.
func MergeContacts(base, extra Contact) Contact {
	merged := base
	merged.NamePrefix = firstNonEmpty(merged.NamePrefix, extra.NamePrefix)
	merged.GivenName = firstNonEmpty(merged.GivenName, extra.GivenName)
	merged.AdditionalName = firstNonEmpty(merged.AdditionalName, extra.AdditionalName)
	merged.FamilyName = firstNonEmpty(merged.FamilyName, extra.FamilyName)
	merged.NameSuffix = firstNonEmpty(merged.NameSuffix, extra.NameSuffix)
	merged.DisplayName = firstNonEmpty(merged.DisplayName, extra.DisplayName)
	merged.Nickname = firstNonEmpty(merged.Nickname, extra.Nickname)
	merged.Organization = firstNonEmpty(merged.Organization, extra.Organization)
	merged.Department = firstNonEmpty(merged.Department, extra.Department)
	merged.JobTitle = firstNonEmpty(merged.JobTitle, extra.JobTitle)
	merged.Birthday = firstNonEmpty(merged.Birthday, extra.Birthday)
	merged.Notes = firstNonEmpty(merged.Notes, extra.Notes)
	merged.Categories = firstNonEmpty(merged.Categories, extra.Categories)
	merged.Emails = MergeContactEmails(merged.Emails, extra.Emails)
	merged.Phones = MergeContactPhones(merged.Phones, extra.Phones)
	merged.Addresses = MergeContactAddresses(merged.Addresses, extra.Addresses)
	merged.URLs = MergeContactURLs(merged.URLs, extra.URLs)
	return merged
}

// MergeContactEmails appends the addresses of incoming that existing lacks.
func MergeContactEmails(existing, incoming []ContactEmail) []ContactEmail {
	seen := map[string]bool{}
	out := append([]ContactEmail{}, existing...)
	for _, email := range existing {
		seen[NormalizeContactEmail(email.Email)] = true
	}
	for _, email := range incoming {
		key := NormalizeContactEmail(email.Email)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, email)
	}
	return out
}

// MergeContactPhones appends the numbers of incoming that existing lacks.
func MergeContactPhones(existing, incoming []ContactPhone) []ContactPhone {
	seen := map[string]bool{}
	out := append([]ContactPhone{}, existing...)
	for _, phone := range existing {
		seen[strings.ToLower(strings.TrimSpace(phone.Number))] = true
	}
	for _, phone := range incoming {
		key := strings.ToLower(strings.TrimSpace(phone.Number))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, phone)
	}
	return out
}

// MergeContactAddresses appends the addresses of incoming that existing lacks.
func MergeContactAddresses(existing, incoming []ContactAddress) []ContactAddress {
	seen := map[string]bool{}
	out := append([]ContactAddress{}, existing...)
	for _, addr := range existing {
		seen[contactAddressKey(addr)] = true
	}
	for _, addr := range incoming {
		key := contactAddressKey(addr)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, addr)
	}
	return out
}

// MergeContactURLs appends the links of incoming that existing lacks.
func MergeContactURLs(existing, incoming []ContactURL) []ContactURL {
	seen := map[string]bool{}
	out := append([]ContactURL{}, existing...)
	for _, u := range existing {
		seen[strings.ToLower(strings.TrimSpace(u.URL))] = true
	}
	for _, u := range incoming {
		key := strings.ToLower(strings.TrimSpace(u.URL))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, u)
	}
	return out
}

// contactAddressKey identifies a postal address by its parts.
func contactAddressKey(addr ContactAddress) string {
	return strings.ToLower(strings.Join([]string{
		strings.TrimSpace(addr.Street),
		strings.TrimSpace(addr.Locality),
		strings.TrimSpace(addr.Region),
		strings.TrimSpace(addr.PostalCode),
		strings.TrimSpace(addr.Country),
	}, "|"))
}
