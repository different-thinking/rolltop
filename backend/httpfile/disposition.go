// File overview: Spelling one `Content-Disposition` header, in the one place
// every route that serves a file by name reads it from.
//
// The header looks trivial and is not. Its plain `filename` parameter is
// ISO-8859-1 by RFC 6266, so writing UTF-8 bytes into it -- which is what
// `fmt.Sprintf("%q", name)` does -- reaches the browser as mojibake for every
// name that is not ASCII: a message carrying `Sprachmemo Ü.m4a` saves as
// `SprachmemoÃ.m4a`, and `メモ.m4a` saves as line noise. The extended
// `filename*` parameter is the one that carries the real name, and the plain
// one stays behind it as the fallback for anything that does not read the
// extended form.
//
// It lives in its own package rather than beside one of its callers because the
// callers are on both sides of the plugin boundary: the core serves attachments
// and blobs, a runtime plugin serves files off remote storage, and a plugin
// cannot import the web package. One copy of a header nobody remembers the
// rules for is worth a package.
package httpfile

import (
	"fmt"
	"path"
	"strings"
)

// Disposition builds the header value for serving one file. Whether the file
// may be rendered in place is the caller's decision -- it depends on the
// content type and on how much the caller trusts where the bytes came from --
// so it arrives already made.
//
// fallback names the file when the name is empty or is not a name at all
// ("attachment", "download"); a caller that passes "" gets "download".
func Disposition(inline bool, filename, fallback string) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	return kind + "; " + Filename(filename, fallback)
}

// Filename builds just the parameters, for a caller that spells the disposition
// itself.
func Filename(filename, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		fallback = "download"
	}
	name := path.Base(strings.TrimSpace(filename))
	// path.Base answers "." for an empty path and "/" for a bare slash, and
	// hands ".." straight back. None of the three is a name, and a filename
	// parameter has to be one.
	if name == "." || name == ".." || name == "/" || name == "" {
		name = fallback
	}
	ascii := asciiFallbackName(name, fallback)
	if ascii == name {
		return fmt.Sprintf("filename=%q", ascii)
	}
	return fmt.Sprintf("filename=%q; filename*=UTF-8''%s", ascii, rfc5987Escape(name))
}

// asciiFallbackName reduces a name to what the plain parameter may carry:
// printable ASCII, with the quote and backslash that would end the quoted
// string replaced rather than escaped -- an escaped quote is legal in a
// quoted-string but not every client parses one, and the fallback's whole job
// is to be understood by the clients that need it.
func asciiFallbackName(name, fallback string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('_')
		case r < 0x20 || r > 0x7e:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	// A name that was entirely non-ASCII reduces to underscores, which says
	// nothing. The caller's fallback at least says what kind of thing it is.
	if strings.Trim(out, "_") == "" {
		return fallback
	}
	return out
}

// rfc5987Escape percent-encodes a name for the `filename*` parameter. The
// unreserved set is the attr-char set RFC 5987 names; everything else --
// including the space, which url.QueryEscape would turn into a plus -- is
// escaped byte by byte, so multi-byte characters survive as their UTF-8 bytes.
func rfc5987Escape(name string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case strings.IndexByte("!#$&+-.^_`|~", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}
