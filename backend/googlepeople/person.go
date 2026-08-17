// File overview: The People API person resource and its translation to and from
// a Rolltop contact. Rolltop's contact model is vCard-shaped and Google's is
// close to it, so this is mostly field pairing -- the interesting parts are the
// asymmetries: Google keeps lists where Rolltop keeps one value, and a write
// that omits a listed field clears it.

package googlepeople

import (
	"fmt"
	"strings"
	"time"

	"rolltop/backend/store"
)

// maxPhotoBytes caps a downloaded contact photo. It matches the limit the
// contact icon upload route enforces, so a synced photo cannot be larger than
// one a user could have uploaded by hand.
const maxPhotoBytes = 2 << 20

// Person is the subset of the People API person resource Rolltop reads or
// writes. Fields it does not model are absent on purpose: a write listing a
// field in updatePersonFields clears it when the payload omits it, so carrying
// unmapped data would be worse than not requesting it.
type Person struct {
	ResourceName string           `json:"resourceName,omitempty"`
	ETag         string           `json:"etag,omitempty"`
	Metadata     *PersonMetadata  `json:"metadata,omitempty"`
	Names        []Name           `json:"names,omitempty"`
	Nicknames    []Nickname       `json:"nicknames,omitempty"`
	Emails       []EmailAddress   `json:"emailAddresses,omitempty"`
	Phones       []PhoneNumber    `json:"phoneNumbers,omitempty"`
	Addresses    []Address        `json:"addresses,omitempty"`
	Orgs         []Organization   `json:"organizations,omitempty"`
	Biographies  []Biography      `json:"biographies,omitempty"`
	Birthdays    []Birthday       `json:"birthdays,omitempty"`
	URLs         []URL            `json:"urls,omitempty"`
	Photos       []Photo          `json:"photos,omitempty"`
	Memberships  []Membership     `json:"memberships,omitempty"`
	Sources      []map[string]any `json:"-"`
}

type PersonMetadata struct {
	Deleted bool             `json:"deleted,omitempty"`
	Sources []MetadataSource `json:"sources,omitempty"`
}

type MetadataSource struct {
	Type       string `json:"type,omitempty"`
	ID         string `json:"id,omitempty"`
	UpdateTime string `json:"updateTime,omitempty"`
}

type FieldMetadata struct {
	Primary bool `json:"primary,omitempty"`
}

type Name struct {
	Metadata       *FieldMetadata `json:"metadata,omitempty"`
	HonorificPre   string         `json:"honorificPrefix,omitempty"`
	GivenName      string         `json:"givenName,omitempty"`
	MiddleName     string         `json:"middleName,omitempty"`
	FamilyName     string         `json:"familyName,omitempty"`
	HonorificSuf   string         `json:"honorificSuffix,omitempty"`
	DisplayName    string         `json:"displayName,omitempty"`
	UnstructuredNm string         `json:"unstructuredName,omitempty"`
}

type Nickname struct {
	Value string `json:"value,omitempty"`
}

type EmailAddress struct {
	Metadata *FieldMetadata `json:"metadata,omitempty"`
	Value    string         `json:"value,omitempty"`
	Type     string         `json:"type,omitempty"`
	Label    string         `json:"formattedType,omitempty"`
}

type PhoneNumber struct {
	Metadata *FieldMetadata `json:"metadata,omitempty"`
	Value    string         `json:"value,omitempty"`
	Type     string         `json:"type,omitempty"`
	Label    string         `json:"formattedType,omitempty"`
}

type Address struct {
	Metadata   *FieldMetadata `json:"metadata,omitempty"`
	Type       string         `json:"type,omitempty"`
	Label      string         `json:"formattedType,omitempty"`
	StreetAddr string         `json:"streetAddress,omitempty"`
	City       string         `json:"city,omitempty"`
	Region     string         `json:"region,omitempty"`
	PostalCode string         `json:"postalCode,omitempty"`
	Country    string         `json:"country,omitempty"`
}

type Organization struct {
	Metadata   *FieldMetadata `json:"metadata,omitempty"`
	Name       string         `json:"name,omitempty"`
	Department string         `json:"department,omitempty"`
	Title      string         `json:"title,omitempty"`
}

type Biography struct {
	Value       string `json:"value,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}

type Birthday struct {
	Date *Date  `json:"date,omitempty"`
	Text string `json:"text,omitempty"`
}

type Date struct {
	Year  int `json:"year,omitempty"`
	Month int `json:"month,omitempty"`
	Day   int `json:"day,omitempty"`
}

type URL struct {
	Metadata *FieldMetadata `json:"metadata,omitempty"`
	Value    string         `json:"value,omitempty"`
	Type     string         `json:"type,omitempty"`
	Label    string         `json:"formattedType,omitempty"`
}

type Photo struct {
	URL string `json:"url,omitempty"`
	// Default marks Google's generated monogram avatar rather than a picture
	// the contact actually has. Importing those would give every contact
	// without a photo a meaningless icon.
	Default bool `json:"default,omitempty"`
}

type Membership struct {
	ContactGroup *ContactGroupMembership `json:"contactGroupMembership,omitempty"`
}

type ContactGroupMembership struct {
	ResourceName string `json:"contactGroupResourceName,omitempty"`
}

// IsDeleted reports whether this entry is a tombstone from a delta response.
func (p Person) IsDeleted() bool {
	return p.Metadata != nil && p.Metadata.Deleted
}

// UpdatedAt is when Google last changed this person, as far as it reports it.
// It is used for display only; the etag is what decides whether a write is safe.
func (p Person) UpdatedAt() time.Time {
	if p.Metadata == nil {
		return time.Time{}
	}
	var newest time.Time
	for _, source := range p.Metadata.Sources {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(source.UpdateTime))
		if err != nil {
			continue
		}
		if parsed.After(newest) {
			newest = parsed.UTC()
		}
	}
	return newest
}

// ToContact translates a person into a Rolltop contact. The connection id and
// the caller's own identity are not filled in here; the sync owns those.
func ToContact(p Person) store.Contact {
	c := store.Contact{
		Source:          store.ContactSourceGoogle,
		ExternalID:      strings.TrimSpace(p.ResourceName),
		ETag:            strings.TrimSpace(p.ETag),
		RemoteUpdatedAt: p.UpdatedAt(),
	}
	if name := primaryName(p.Names); name != nil {
		c.NamePrefix = strings.TrimSpace(name.HonorificPre)
		c.GivenName = strings.TrimSpace(name.GivenName)
		c.AdditionalName = strings.TrimSpace(name.MiddleName)
		c.FamilyName = strings.TrimSpace(name.FamilyName)
		c.NameSuffix = strings.TrimSpace(name.HonorificSuf)
		c.DisplayName = strings.TrimSpace(name.DisplayName)
		if c.DisplayName == "" {
			c.DisplayName = strings.TrimSpace(name.UnstructuredNm)
		}
	}
	if len(p.Nicknames) > 0 {
		c.Nickname = strings.TrimSpace(p.Nicknames[0].Value)
	}
	if org := primaryOrganization(p.Orgs); org != nil {
		c.Organization = strings.TrimSpace(org.Name)
		c.Department = strings.TrimSpace(org.Department)
		c.JobTitle = strings.TrimSpace(org.Title)
	}
	if len(p.Biographies) > 0 {
		c.Notes = strings.TrimSpace(p.Biographies[0].Value)
	}
	c.Birthday = birthdayString(p.Birthdays)
	for _, email := range p.Emails {
		value := strings.TrimSpace(email.Value)
		if value == "" {
			continue
		}
		c.Emails = append(c.Emails, store.ContactEmail{
			Label:     contactLabel(email.Label, email.Type, "Email"),
			Email:     value,
			IsPrimary: isPrimary(email.Metadata),
		})
	}
	for _, phone := range p.Phones {
		value := strings.TrimSpace(phone.Value)
		if value == "" {
			continue
		}
		c.Phones = append(c.Phones, store.ContactPhone{
			Label:     contactLabel(phone.Label, phone.Type, "Phone"),
			Number:    value,
			IsPrimary: isPrimary(phone.Metadata),
		})
	}
	for _, addr := range p.Addresses {
		entry := store.ContactAddress{
			Label:      contactLabel(addr.Label, addr.Type, "Address"),
			Street:     strings.TrimSpace(addr.StreetAddr),
			Locality:   strings.TrimSpace(addr.City),
			Region:     strings.TrimSpace(addr.Region),
			PostalCode: strings.TrimSpace(addr.PostalCode),
			Country:    strings.TrimSpace(addr.Country),
			IsPrimary:  isPrimary(addr.Metadata),
		}
		if entry.Street == "" && entry.Locality == "" && entry.Region == "" && entry.PostalCode == "" && entry.Country == "" {
			continue
		}
		c.Addresses = append(c.Addresses, entry)
	}
	for _, u := range p.URLs {
		value := strings.TrimSpace(u.Value)
		if value == "" {
			continue
		}
		c.URLs = append(c.URLs, store.ContactURL{
			Label:     contactLabel(u.Label, u.Type, "Website"),
			URL:       value,
			IsPrimary: isPrimary(u.Metadata),
		})
	}
	return c
}

// FromContact builds the payload for a write. Every field listed in
// WritePersonFields is emitted even when empty, because Google clears a listed
// field whose value is absent -- which is exactly what deleting the last phone
// number in Rolltop has to mean at Google.
func FromContact(c store.Contact) Person {
	p := Person{
		ResourceName: strings.TrimSpace(c.ExternalID),
		ETag:         strings.TrimSpace(c.ETag),
		Names:        []Name{},
		Nicknames:    []Nickname{},
		Emails:       []EmailAddress{},
		Phones:       []PhoneNumber{},
		Addresses:    []Address{},
		Orgs:         []Organization{},
		Biographies:  []Biography{},
		Birthdays:    []Birthday{},
		URLs:         []URL{},
	}
	name := Name{
		HonorificPre: strings.TrimSpace(c.NamePrefix),
		GivenName:    strings.TrimSpace(c.GivenName),
		MiddleName:   strings.TrimSpace(c.AdditionalName),
		FamilyName:   strings.TrimSpace(c.FamilyName),
		HonorificSuf: strings.TrimSpace(c.NameSuffix),
	}
	if name != (Name{}) {
		p.Names = append(p.Names, name)
	} else if display := strings.TrimSpace(c.DisplayName); display != "" {
		// Google derives displayName itself and rejects it as an input, so a
		// contact that only ever had a display name travels as an
		// unstructured one.
		p.Names = append(p.Names, Name{UnstructuredNm: display})
	}
	if nickname := strings.TrimSpace(c.Nickname); nickname != "" {
		p.Nicknames = append(p.Nicknames, Nickname{Value: nickname})
	}
	org := Organization{
		Name:       strings.TrimSpace(c.Organization),
		Department: strings.TrimSpace(c.Department),
		Title:      strings.TrimSpace(c.JobTitle),
	}
	if org != (Organization{}) {
		p.Orgs = append(p.Orgs, org)
	}
	if notes := strings.TrimSpace(c.Notes); notes != "" {
		p.Biographies = append(p.Biographies, Biography{Value: notes, ContentType: "TEXT_PLAIN"})
	}
	if birthday := parseBirthday(c.Birthday); birthday != nil {
		p.Birthdays = append(p.Birthdays, *birthday)
	}
	for _, email := range c.Emails {
		value := strings.TrimSpace(email.Email)
		if value == "" {
			continue
		}
		p.Emails = append(p.Emails, EmailAddress{Value: value, Type: googleType(email.Label)})
	}
	for _, phone := range c.Phones {
		value := strings.TrimSpace(phone.Number)
		if value == "" {
			continue
		}
		p.Phones = append(p.Phones, PhoneNumber{Value: value, Type: googleType(phone.Label)})
	}
	for _, addr := range c.Addresses {
		entry := Address{
			Type:       googleType(addr.Label),
			StreetAddr: strings.TrimSpace(addr.Street),
			City:       strings.TrimSpace(addr.Locality),
			Region:     strings.TrimSpace(addr.Region),
			PostalCode: strings.TrimSpace(addr.PostalCode),
			Country:    strings.TrimSpace(addr.Country),
		}
		if entry.StreetAddr == "" && entry.City == "" && entry.Region == "" && entry.PostalCode == "" && entry.Country == "" {
			continue
		}
		p.Addresses = append(p.Addresses, entry)
	}
	for _, u := range c.URLs {
		value := strings.TrimSpace(u.URL)
		if value == "" {
			continue
		}
		p.URLs = append(p.URLs, URL{Value: value, Type: googleType(u.Label)})
	}
	return p
}

// PrimaryPhotoURL returns the contact's real photo, or "" when Google only has
// its generated placeholder.
func PrimaryPhotoURL(p Person) string {
	for _, photo := range p.Photos {
		if photo.Default {
			continue
		}
		if url := strings.TrimSpace(photo.URL); url != "" {
			return url
		}
	}
	return ""
}

func primaryName(names []Name) *Name {
	for i := range names {
		if isPrimary(names[i].Metadata) {
			return &names[i]
		}
	}
	if len(names) > 0 {
		return &names[0]
	}
	return nil
}

func primaryOrganization(orgs []Organization) *Organization {
	for i := range orgs {
		if isPrimary(orgs[i].Metadata) {
			return &orgs[i]
		}
	}
	if len(orgs) > 0 {
		return &orgs[0]
	}
	return nil
}

func isPrimary(meta *FieldMetadata) bool {
	return meta != nil && meta.Primary
}

// contactLabel prefers the label Google already formatted for display, falls
// back to the machine type, and only then to a generic word, so a custom label
// the user typed at Google survives the round trip.
func contactLabel(formatted, machine, fallback string) string {
	if label := strings.TrimSpace(formatted); label != "" {
		return label
	}
	if label := strings.TrimSpace(machine); label != "" {
		return strings.ToUpper(label[:1]) + label[1:]
	}
	return fallback
}

// genericLabels are the words contactLabel invents when Google supplied no type
// at all. Sending one back would turn "no label" into a custom label reading
// "Email" on every address the contact has.
var genericLabels = map[string]bool{"email": true, "phone": true, "address": true, "website": true}

// googleType maps a Rolltop label onto a People API type. Google's own values
// are matched case-insensitively so a round trip does not rewrite them, and
// anything else travels as-is: the type field accepts a custom string, and
// dropping it would silently delete a label the user typed at Google -- an
// "School" address would come back unlabelled after an unrelated edit.
func googleType(label string) string {
	trimmed := strings.TrimSpace(label)
	switch lowered := strings.ToLower(trimmed); {
	case lowered == "":
		return ""
	case genericLabels[lowered]:
		return ""
	case lowered == "home":
		return "home"
	case lowered == "work", lowered == "office":
		return "work"
	case lowered == "mobile", lowered == "cell":
		return "mobile"
	case lowered == "other":
		return "other"
	}
	return trimmed
}

// birthdayString renders Google's birthday as the ISO-ish string the contact
// model stores. Google allows a year-less birthday, which vCard writes as
// --MM-DD, and dropping the month/day pair to avoid the odd shape would lose
// the only part the user cares about.
func birthdayString(birthdays []Birthday) string {
	for _, birthday := range birthdays {
		if birthday.Date == nil {
			if text := strings.TrimSpace(birthday.Text); text != "" {
				return text
			}
			continue
		}
		d := birthday.Date
		if d.Month <= 0 || d.Day <= 0 {
			continue
		}
		if d.Year <= 0 {
			return fmt.Sprintf("--%02d-%02d", d.Month, d.Day)
		}
		return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
	}
	return ""
}

// parseBirthday is birthdayString in reverse. An unparseable value travels as
// free text so a hand-typed birthday is not silently discarded on write-back.
func parseBirthday(value string) *Birthday {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "--") {
		var month, day int
		if _, err := fmt.Sscanf(value, "--%02d-%02d", &month, &day); err == nil && month > 0 && day > 0 {
			return &Birthday{Date: &Date{Month: month, Day: day}}
		}
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return &Birthday{Date: &Date{Year: parsed.Year(), Month: int(parsed.Month()), Day: parsed.Day()}}
	}
	return &Birthday{Text: value}
}
