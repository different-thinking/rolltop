// File overview: Downloading every attachment of one message as a ZIP archive.

package web

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"rolltop/backend/mailparse"
	"rolltop/backend/store"
)

// attachmentArchiveRoutePrefix addresses a message, not an attachment:
// /attachments/message/<message-id>/zip. It is registered as its own pattern
// so ServeMux routes it ahead of the per-attachment "/attachments/" subtree.
const attachmentArchiveRoutePrefix = "/attachments/message/"

// attachmentArchiveEntry is one file of the archive, resolved before anything
// is written: once the ZIP stream has started the status is sent, so a part
// that cannot be produced has to be found while a clear error is still
// possible.
type attachmentArchiveEntry struct {
	name        string
	contentType string
	file        io.ReadCloser
	data        []byte
}

// handleAttachmentArchive serves the attachments the message view lists - the
// same visibleAttachments set, so inline images and signature pictures are not
// packed - as one ZIP. Each part is read exactly as the single download reads
// it: the standalone blob when it opens, otherwise the part extracted from the
// raw message, which is parsed at most once for the whole archive.
func (s *Server) handleAttachmentArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAuth(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie")
	messageID, ok := idFromPath(strings.TrimSuffix(r.URL.Path, "/zip"), attachmentArchiveRoutePrefix)
	if !ok || !strings.HasSuffix(r.URL.Path, "/zip") {
		http.NotFound(w, r)
		return
	}
	msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, messageID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	all, err := s.store.ListAttachmentsForMessage(r.Context(), cu.User.ID, msg.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	attachments := visibleAttachments(all)
	if len(attachments) == 0 {
		http.Error(w, "message has no attachments", http.StatusNotFound)
		return
	}

	entries := make([]attachmentArchiveEntry, 0, len(attachments))
	defer func() {
		for _, entry := range entries {
			if entry.file != nil {
				_ = entry.file.Close()
			}
		}
	}()
	names := attachmentArchiveNames{}
	var parsed *mailparse.ParsedMessage
	// The parts not yet handed to a row. Two rows with the same filename and
	// size would otherwise both resolve to the first such part, and the
	// archive would carry one file twice and the other not at all.
	var unclaimed []mailparse.Attachment
	for _, att := range attachments {
		entry := attachmentArchiveEntry{name: names.next(att), contentType: att.ContentType}
		if strings.TrimSpace(att.BlobPath) != "" && s.blobs != nil {
			file, err := s.blobs.OpenUserBlob(cu.User.ID, att.BlobPath)
			if err == nil {
				entry.file = file
				entries = append(entries, entry)
				continue
			}
			log.Printf("attachment archive blob unavailable, falling back to the raw message user_id=%d attachment_id=%d path=%q: %v", cu.User.ID, att.ID, att.BlobPath, err)
		}
		if parsed == nil {
			raw, err := s.rawMessageBytes(r.Context(), cu.User.ID, msg)
			if err != nil {
				http.Error(w, "attachment bodies are not available locally and could not be fetched from IMAP", http.StatusGone)
				return
			}
			message, err := mailparse.Parse(raw)
			if err != nil {
				http.Error(w, "attachment bodies could not be parsed from the message", http.StatusGone)
				return
			}
			parsed = &message
			unclaimed = append([]mailparse.Attachment(nil), parsed.Files...)
		}
		index := matchingAttachmentIndex(att, unclaimed)
		if index < 0 {
			attachmentUnavailable(w, cu.User.ID, att.ID, fmt.Sprintf("archive: no matching part in the raw message (content_id=%q filename=%q content_type=%q parts=%d)", att.ContentID, att.Filename, att.ContentType, len(parsed.Files)))
			return
		}
		file := unclaimed[index]
		unclaimed = append(unclaimed[:index], unclaimed[index+1:]...)
		entry.data = file.Data
		if strings.TrimSpace(file.ContentType) != "" {
			entry.contentType = file.ContentType
		}
		entries = append(entries, entry)
	}

	modified := attachmentArchiveModified(msg.Date, msg.InternalDate, time.Now())
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", attachmentArchiveDisposition(msg.Subject))
	zw := zip.NewWriter(w)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: attachmentArchiveMethod(entry.contentType), Modified: modified}
		dst, err := zw.CreateHeader(header)
		if err != nil {
			log.Printf("attachment archive write failed user_id=%d message_id=%d: %v", cu.User.ID, msg.ID, err)
			return
		}
		var src io.Reader = bytes.NewReader(entry.data)
		if entry.file != nil {
			src = entry.file
		}
		if _, err := io.Copy(dst, src); err != nil {
			// The status is already on the wire; leaving the archive without
			// its central directory makes the browser report a failed download
			// rather than save a ZIP that silently lacks a file.
			log.Printf("attachment archive write failed user_id=%d message_id=%d: %v", cu.User.ID, msg.ID, err)
			return
		}
	}
	if err := zw.Close(); err != nil {
		log.Printf("attachment archive close failed user_id=%d message_id=%d: %v", cu.User.ID, msg.ID, err)
	}
}

// attachmentArchiveNames hands out one entry name per attachment. A message
// may carry two parts with the same filename, and ZIP tools either refuse such
// an archive or let the second overwrite the first on extraction, so repeats
// are numbered the way a browser numbers a repeated download.
type attachmentArchiveNames map[string]bool

func (n attachmentArchiveNames) next(att store.Attachment) string {
	base := attachmentArchiveFilename(att.Filename)
	if base == "" {
		base = "attachment" + attachmentArchiveExtension(att.ContentType)
	}
	name := base
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 2; n[strings.ToLower(name)]; i++ {
		name = stem + " (" + strconv.Itoa(i) + ")" + ext
	}
	n[strings.ToLower(name)] = true
	return name
}

// attachmentArchiveFilename reduces a sender-supplied filename to one path
// element: an entry named "../x" or "C:\x" would write outside the folder the
// reader extracts into with tools that do not guard against it.
func attachmentArchiveFilename(filename string) string {
	name := strings.TrimSpace(filename)
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" || name == "/" || name == "." {
		return ""
	}
	return name
}

// attachmentArchiveDisposition names the download after the message subject.
// mime.FormatMediaType writes a non-ASCII subject as an RFC 2231 filename*,
// which every current browser reads.
func attachmentArchiveDisposition(subject string) string {
	// A subject is not a path: "Invoice 2024/05" names the whole archive, not
	// "05", so its separators are replaced rather than resolved.
	name := attachmentArchiveFilename(strings.NewReplacer("/", "_", "\\", "_").Replace(subject))
	if runes := []rune(name); len(runes) > 80 {
		name = strings.TrimSpace(string(runes[:80]))
	}
	if name == "" {
		name = "attachments"
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"})
	if disposition == "" {
		return `attachment; filename="attachments.zip"`
	}
	return disposition
}

// attachmentArchiveExtension names an unnamed part by its type. The mime
// table lists every extension for a type in alphabetical order, which makes
// text/plain ".asc" and image/jpeg ".jfif", so the usual ones are named first.
func attachmentArchiveExtension(contentType string) string {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(contentType))
	}
	switch mediaType {
	case "text/plain":
		return ".txt"
	case "text/html":
		return ".html"
	case "image/jpeg":
		return ".jpg"
	case "image/tiff":
		return ".tif"
	case "text/calendar":
		return ".ics"
	case "message/rfc822":
		return ".eml"
	}
	if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
		return exts[0]
	}
	return ""
}

// attachmentArchiveMethod stores what is already compressed instead of
// deflating it a second time for nothing.
func attachmentArchiveMethod(contentType string) uint16 {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(contentType))
	}
	switch {
	case strings.HasPrefix(mediaType, "video/"), strings.HasPrefix(mediaType, "audio/"):
		return zip.Store
	case strings.HasPrefix(mediaType, "application/vnd.openxmlformats-"),
		strings.HasPrefix(mediaType, "application/vnd.oasis.opendocument."):
		return zip.Store
	}
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/heic", "image/avif",
		"application/zip", "application/x-zip-compressed", "application/gzip", "application/x-gzip",
		"application/x-7z-compressed", "application/x-rar-compressed", "application/vnd.rar",
		"application/x-bzip2", "application/x-xz", "application/zstd":
		return zip.Store
	}
	return zip.Deflate
}

// attachmentArchiveModified dates the entries by the message. The Date header
// is the sender's to write, and a ZIP entry carries an MS-DOS date that cannot
// say anything before 1980 or after 2107 - outside that range it wraps into a
// nonsense date - so a date it cannot hold falls back to the next one.
func attachmentArchiveModified(candidates ...time.Time) time.Time {
	for _, t := range candidates {
		if year := t.Year(); !t.IsZero() && year >= 1980 && year <= 2107 {
			return t
		}
	}
	return time.Now()
}
