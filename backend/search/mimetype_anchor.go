// File overview: Making `mimetype:` mean the same thing on both search
// backends.
//
// The PostgreSQL backend answers the operator with an anchored `ILIKE` against
// `attachments.content_type`, so `mimetype:audio/` selects a type family and
// nothing else. Bleve cannot do that against `attachment_types` as it stands:
// the field is analyzed, so `audio/mpeg` is indexed as the two ordinary tokens
// `audio` and `mpeg`, and a query for the family can only ask for the token
// `audio` -- which `application/x-audio-playlist` carries just as happily.
//
// The fix is to put the anchoring into the indexed text rather than into the
// query: alongside the content types, each document also carries one synthetic
// token per type family and per exact type. Those tokens are unambiguous, so a
// term query against them is exactly as precise as the SQL `ILIKE`.
//
// What makes this safe to ship without forcing a reindex is the marker token.
// A document indexed since this existed carries it; one indexed before does
// not. The query is therefore "the anchored token, OR the old loose phrase on a
// document that has no anchored tokens to offer" -- so mail indexed by an older
// build keeps matching exactly as well as it did, and mail indexed since matches
// precisely. Precision improves as documents are reindexed, and nothing has to
// be rebuilt for the operator to work at all.

package search

import (
	"strings"
	"unicode"
)

// The synthetic tokens are prefixed rather than spelled readably on purpose.
// `attachment_types` is one of the fields plain free text searches, and the
// last term of a search is prefix-matched for as-you-type results -- so a
// readable prefix like `mime` would put every message with an attachment into
// the results for someone typing the word "mime". `zmt` is not a word anyone
// types.
const (
	mimeAnchorPrefix = "zmt_"
	// mimeAnchorMarker says this document carries anchored tokens. It
	// deliberately does not start with the family prefix, so no content type can
	// ever produce it.
	mimeAnchorMarker = "zmtanchored"
)

// mimeTypeIndexValues returns what goes into `attachment_types`: the content
// types themselves, then one anchored token per family and per exact type, then
// the marker.
//
// The order is the load-bearing part. The caller bounds the joined result by
// byte length, and a truncation that reaches the marker also reached the tokens
// before it -- so a document that kept the marker kept all of them, and one
// that lost it reads as a document from before this existed and is answered by
// the loose fallback. A truncation can therefore cost precision but can never
// cost a match.
func mimeTypeIndexValues(contentTypes []string) []string {
	anchored := make([]string, 0, len(contentTypes)*2+1)
	seen := map[string]bool{}
	for _, entry := range contentTypes {
		// One entry can hold several types already joined into a string. The
		// pending-batch bounding collapses a message's attachments into a
		// single doc whose ContentType is the joined value, and the projection
		// has to derive the same tokens from either shape or a bounded document
		// stops indexing like the unbounded one it replaces. A content type
		// never contains whitespace, so splitting on it is exact.
		for _, contentType := range strings.Fields(entry) {
			for _, token := range mimeAnchorTokens(contentType) {
				if seen[token] {
					continue
				}
				seen[token] = true
				anchored = append(anchored, token)
			}
		}
	}
	if len(anchored) == 0 {
		return contentTypes
	}
	out := make([]string, 0, len(contentTypes)+len(anchored)+1)
	out = append(out, contentTypes...)
	out = append(out, anchored...)
	return append(out, mimeAnchorMarker)
}

// mimeAnchorTokens returns the family token and, when the type has a subtype,
// the exact token for one content type.
func mimeAnchorTokens(contentType string) []string {
	mainType, subType := splitMIMEType(contentType)
	if mainType == "" {
		return nil
	}
	out := []string{mimeAnchorPrefix + mainType}
	if subType != "" {
		out = append(out, mimeAnchorPrefix+mainType+"_"+subType)
	}
	return out
}

// mimeAnchorQueryToken is the token a `mimetype:` operand asks for. A bare
// family -- `audio` or `audio/` -- asks for the family token; a full type asks
// for the exact one.
func mimeAnchorQueryToken(value string) string {
	mainType, subType := splitMIMEType(value)
	if mainType == "" {
		return ""
	}
	if subType == "" {
		return mimeAnchorPrefix + mainType
	}
	return mimeAnchorPrefix + mainType + "_" + subType
}

// splitMIMEType reduces a content type to its type and subtype, each stripped
// to letters and digits.
//
// Stripping is what keeps the two sides in step: `image/svg+xml` and
// `application/vnd.ms-excel` have to reduce identically whether they arrive
// from a stored header or from a search box, and the separator between the two
// halves has to be the one character the text analyzer keeps inside a word --
// an underscore. Everything else would split the token in two and defeat the
// anchoring.
func splitMIMEType(value string) (string, string) {
	value = strings.ToLower(strings.TrimSpace(value))
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	mainType, subType, _ := strings.Cut(value, "/")
	return alphanumeric(mainType), alphanumeric(subType)
}

func alphanumeric(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
