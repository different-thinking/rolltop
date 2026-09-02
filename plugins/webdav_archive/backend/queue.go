// File overview: What turns mail into files on a WebDAV server -- the hook that
// notices an attachment worth keeping, the queue row it writes, and the worker
// that carries the bytes across.
//
// The split matters. Noticing happens inline with mail sync, where anything
// slow or fallible would hold up the mirror, so the hook does the cheap part:
// it reads attachment metadata that is already in the database and writes a
// row. Everything that can fail -- fetching the raw message, reaching a server
// that may be switched off, the upload itself -- happens in the worker, where a
// failure is a retry rather than a message that did not get stored.

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/mailparse"
	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

// workerInterval is how often the queue is swept when nothing has woken it.
// New mail wakes the worker directly, so this covers the other case: a server
// that was unreachable and whose retries are now due.
const workerInterval = time.Minute

// uploadBatch bounds one user's turn, so a queue with a thousand rows in it
// does not hold the worker on one account while another waits.
const uploadBatch = 20

// ImportStoredMessage is the sync-time hook. It runs for every mirrored
// message, so its cost when there is nothing to do -- the common case by far --
// is one indexed query returning no rows.
func (p *webdavArchiveBackend) ImportStoredMessage(ctx context.Context, host plugins.StoredMessageHost, msg plugins.StoredMessageContext) error {
	st, ok := host.Store().(*store.Store)
	if !ok || st == nil {
		return plugins.ErrUnsupported
	}
	db, err := st.UserDB(ctx, msg.UserID)
	if err != nil {
		return err
	}
	targets, err := listTargetsWatching(ctx, db, msg.UserID, msg.MailboxID)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	attachments, err := st.ListAttachmentsForMessage(ctx, msg.UserID, msg.MessageID)
	if err != nil {
		return err
	}
	if len(attachments) == 0 {
		return nil
	}
	queued := 0
	for _, item := range targets {
		for index, attachment := range attachments {
			if attachment.IsInline && !item.IncludeInline {
				continue
			}
			if !item.matchesContentType(attachment.ContentType) {
				continue
			}
			added, err := enqueueUpload(ctx, db, upload{
				UserID:    msg.UserID,
				TargetID:  item.ID,
				MessageID: msg.MessageID,
				// The row id names the part, and its position backs that up:
				// the store writes attachment rows in parse order, so the index
				// here is the part's place in the message and is what pairs the
				// row back to a MIME part when the metadata cannot.
				AttachmentID:    attachment.ID,
				AttachmentIndex: index,
				Filename:        attachment.Filename,
				ContentType:     attachment.ContentType,
				Size:            attachment.Size,
				Subject:         msg.Subject,
				FromAddr:        msg.From,
				MessageDate:     msg.Date,
			})
			if err != nil {
				return err
			}
			if added {
				queued++
			}
		}
	}
	if queued > 0 {
		p.wake()
	}
	return nil
}

// worker is the queue's own goroutine. It holds no per-user state: what is owed
// lives in the table, which is what lets a restart pick up exactly where the
// last process stopped.
type worker struct {
	host  plugins.BackendStartHost
	store *store.Store
	ctx   context.Context
	stop  context.CancelFunc
	wake  chan struct{}
	done  chan struct{}
}

func newWorker(host plugins.BackendStartHost, st *store.Store) *worker {
	// The process lifetime, when the host offers one, so a shutdown interrupts
	// an upload rather than letting it run on into a closed database.
	parent := context.Background()
	if lifecycle, ok := host.(plugins.LifecycleHost); ok {
		if lifetime := lifecycle.Lifetime(); lifetime != nil {
			parent = lifetime
		}
	}
	ctx, cancel := context.WithCancel(parent)
	return &worker{
		host:  host,
		store: st,
		ctx:   ctx,
		stop:  cancel,
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
}

func (w *worker) Start() {
	go w.loop()
}

func (w *worker) Stop() {
	w.stop()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
	}
}

// Wake asks for a sweep now. It never blocks: the channel holds one token, and
// a wake arriving while one is already pending is the same request.
func (w *worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *worker) loop() {
	defer close(w.done)
	// A previous process may have stopped with rows marked uploading. They are
	// owed, not done, so they go back on the queue before the first sweep.
	if db, err := w.store.UserDB(w.ctx, 0); err == nil {
		if err := releaseInterruptedUploads(w.ctx, db); err != nil {
			log.Printf("webdav archive could not release interrupted uploads error_type=%T", err)
		}
	}
	ticker := time.NewTicker(workerInterval)
	defer ticker.Stop()
	w.sweep()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.sweep()
		case <-w.wake:
			w.sweep()
		}
	}
}

func (w *worker) sweep() {
	now := time.Now().UTC()
	db, err := w.store.UserDB(w.ctx, 0)
	if err != nil {
		return
	}
	userIDs, err := usersWithWork(w.ctx, db, now)
	if err != nil {
		log.Printf("webdav archive could not list pending work error_type=%T", err)
		return
	}
	for _, userID := range userIDs {
		if w.ctx.Err() != nil {
			return
		}
		w.runUser(userID)
	}
}

func (w *worker) runUser(userID int64) {
	db, err := w.store.UserDB(w.ctx, userID)
	if err != nil {
		return
	}
	items, err := claimDueUploads(w.ctx, db, userID, time.Now().UTC(), uploadBatch)
	if err != nil {
		log.Printf("webdav archive could not claim uploads user_id=%d error_type=%T", userID, err)
		return
	}
	// One client per target rather than per upload: a batch is usually several
	// recordings from the same folder going to the same server.
	clients := map[int64]*webdavClient{}
	targets := map[int64]target{}
	for _, item := range items {
		if w.ctx.Err() != nil {
			return
		}
		if err := w.runUpload(db, item, clients, targets); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			if failErr := failUpload(w.ctx, db, item, err, time.Now().UTC()); failErr != nil {
				log.Printf("webdav archive could not record a failed upload user_id=%d upload_id=%d error_type=%T",
					userID, item.ID, failErr)
			}
			_ = recordTargetResult(w.ctx, db, userID, item.TargetID, false, err.Error())
		}
	}
}

func (w *worker) runUpload(db *sql.DB, item upload, clients map[int64]*webdavClient, targets map[int64]target) error {
	client, ok := clients[item.TargetID]
	if !ok {
		configured, err := getTarget(w.ctx, db, item.UserID, item.TargetID)
		if err != nil {
			return fmt.Errorf("the target this upload belongs to is gone: %w", err)
		}
		password := ""
		if strings.TrimSpace(configured.EncryptedPassword) != "" {
			password, err = mmcrypto.DecryptString(w.host.MasterKey(), configured.EncryptedPassword)
			if err != nil {
				return fmt.Errorf("the stored WebDAV password could not be read: %w", err)
			}
		}
		client, err = newWebDAVClient(configured.BaseURL, configured.Username, password)
		if err != nil {
			return err
		}
		clients[item.TargetID] = client
		targets[item.TargetID] = configured
	}
	configured := targets[item.TargetID]
	data, contentType, err := w.attachmentBytes(item)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	item.Size = int64(len(data))
	if contentType != "" {
		item.ContentType = contentType
	}
	// The same bytes already on the server for this target are not uploaded
	// again; the row records where they went, so the reader can still find the
	// file this mail carried.
	existing, err := duplicateUploadPath(w.ctx, db, item.UserID, item.TargetID, hash, item.ID)
	if err != nil {
		return err
	}
	if existing != "" {
		return completeUpload(w.ctx, db, item, statusDuplicate, existing, hash)
	}
	remotePath, err := w.freeRemotePath(client, configured, item, hash)
	if err != nil {
		return err
	}
	// The path is written down before the upload, not after it. A previous
	// attempt that uploaded and then failed to record it would otherwise pick a
	// second path on the retry -- the first one now being taken -- and leave two
	// copies of the same recording behind.
	if err := reserveUploadPath(w.ctx, db, item, remotePath, hash); err != nil {
		return err
	}
	if err := client.Put(w.ctx, remotePath, data, item.ContentType); err != nil {
		return err
	}
	if err := completeUpload(w.ctx, db, item, statusDone, remotePath, hash); err != nil {
		return err
	}
	// The upload is done and the row says so. What is left is a counter on the
	// target, and its failure is not this upload's failure: returning it would
	// send the caller to failUpload, which would rewind a row that is already
	// `done` back to `failed` -- and the retry would then upload nothing and
	// count the same file twice. Logged and dropped instead.
	if err := recordTargetResult(w.ctx, db, item.UserID, item.TargetID, true, ""); err != nil {
		log.Printf("webdav archive could not record a target's success user_id=%d target_id=%d error_type=%T",
			item.UserID, item.TargetID, err)
	}
	return nil
}

// attachmentBytes reads one attachment's content back out of the message it
// arrived in. Attachment bodies are not stored separately -- the row points at
// the raw message -- so this is a fetch and a re-parse rather than a file read.
func (w *worker) attachmentBytes(item upload) ([]byte, string, error) {
	rawHost, ok := w.host.(plugins.RawMessageFetchHost)
	if !ok {
		return nil, "", errors.New("this Rolltop cannot hand raw messages to plugins")
	}
	raw, err := rawHost.FetchRawMessage(w.ctx, item.UserID, item.MessageID)
	if err != nil {
		return nil, "", fmt.Errorf("the message this attachment came in could not be read: %w", err)
	}
	parsed, err := mailparse.Parse(raw)
	if err != nil {
		return nil, "", fmt.Errorf("the message could not be parsed: %w", err)
	}
	file, ok := matchAttachment(item, parsed.Files)
	if !ok {
		return nil, "", errors.New("the attachment is no longer in the message it arrived in")
	}
	if len(file.Data) == 0 {
		return nil, "", errors.New("the attachment is empty")
	}
	return file.Data, strings.TrimSpace(file.ContentType), nil
}

// matchAttachment finds the MIME part a queue row names. The row was written
// from database metadata and the parts come from a fresh parse, so they have to
// be paired on what both carry.
//
// The tiers are the store's, deliberately: store.ReplaceAttachmentsForMessage
// pairs rows to parts the same way, and it documents why a filename-only pass
// is not among them -- matching by name alone reaches across positions and can
// pair two same-named parts of different sizes, handing one part's identity the
// other's bytes. A phone that names every recording `recording.m4a` is exactly
// the case that produces two such parts, so this archive is the last place that
// should guess by name.
//
// What is left when the metadata cannot tell the parts apart is the part's
// position, which is what the row recorded. It is taken only when the part
// found there is the same kind of thing the row describes: a position that now
// holds a different content type means the message was parsed into something
// this row no longer describes, and uploading that would file the wrong file
// under the right name.
func matchAttachment(item upload, files []mailparse.Attachment) (mailparse.Attachment, bool) {
	name := strings.TrimSpace(item.Filename)
	wanted := normalizeContentType(item.ContentType)
	// Filename, size and content type together: an exact metadata match.
	for _, file := range files {
		if name != "" && strings.EqualFold(strings.TrimSpace(file.Filename), name) &&
			int64(len(file.Data)) == item.Size && normalizeContentType(file.ContentType) == wanted {
			return file, true
		}
	}
	// Filename and size: size is the strong discriminator, so this still pins
	// the bytes even when a better parse decoded the content type differently.
	for _, file := range files {
		if name != "" && strings.EqualFold(strings.TrimSpace(file.Filename), name) &&
			int64(len(file.Data)) == item.Size {
			return file, true
		}
	}
	// Content type and size, which is what reaches a part carrying no filename.
	for _, file := range files {
		if wanted != "" && normalizeContentType(file.ContentType) == wanted &&
			item.Size > 0 && int64(len(file.Data)) == item.Size {
			return file, true
		}
	}
	// Position, type-checked.
	if index := item.AttachmentIndex; index >= 0 && index < len(files) {
		file := files[index]
		if wanted == "" || normalizeContentType(file.ContentType) == wanted {
			return file, true
		}
	}
	return mailparse.Attachment{}, false
}

func normalizeContentType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	return value
}

// freeRemotePath is where these bytes will land. The template decides the
// shape; this decides what happens when something is already there. Two
// different recordings that render to the same name -- `recording.m4a` twice,
// which is what a phone sends -- must not overwrite each other, so the second
// takes a suffix from its own content hash.
//
// A file already there whose bytes are these bytes is not a collision: the
// previous attempt uploaded it and failed before it could say so, and writing
// it again is the same file.
func (w *worker) freeRemotePath(client *webdavClient, configured target, item upload, hash string) (string, error) {
	// A path this row already claimed is where it belongs, whatever is there
	// now: a retry writes over its own earlier attempt rather than beside it.
	if reserved := strings.TrimSpace(item.RemotePath); reserved != "" && item.ContentHash == hash {
		return reserved, nil
	}
	base := renderRemotePath(configured.PathTemplate, item)
	exists, err := client.Exists(w.ctx, base)
	if err != nil {
		return "", err
	}
	if !exists {
		return base, nil
	}
	suffixed := appendPathSuffix(base, hash[:8])
	// Whether the suffixed name is free or holds these same bytes, it is where
	// this upload belongs: the suffix is derived from the content, so a name
	// that is taken is taken by this content.
	return suffixed, nil
}

// appendPathSuffix puts a marker before the extension, so `2026/05/memo.m4a`
// becomes `2026/05/memo-1f4a2b3c.m4a` rather than losing the extension the
// player needs.
func appendPathSuffix(remotePath, suffix string) string {
	dir, file := path.Split(remotePath)
	ext := path.Ext(file)
	stem := strings.TrimSuffix(file, ext)
	return dir + stem + "-" + suffix + ext
}

// renderRemotePath fills a target's template. Every substituted value is
// reduced to something safe in a path segment first, so a subject line with a
// slash in it cannot invent a directory and `..` cannot climb out of the store.
func renderRemotePath(template string, item upload) string {
	template = strings.TrimSpace(template)
	if template == "" {
		template = "{yyyy}/{mm}/{filename}"
	}
	when := item.MessageDate
	if when.IsZero() {
		when = item.CreatedAt
	}
	if when.IsZero() {
		when = time.Now().UTC()
	}
	when = when.UTC()
	filename := safeSegment(item.Filename)
	if filename == "" {
		filename = fmt.Sprintf("attachment-%d", item.AttachmentID)
	}
	ext := strings.TrimPrefix(path.Ext(filename), ".")
	replacements := []string{
		"{yyyy}", when.Format("2006"),
		"{mm}", when.Format("01"),
		"{dd}", when.Format("02"),
		"{date}", when.Format("2006-01-02"),
		"{time}", when.Format("150405"),
		"{filename}", filename,
		"{basename}", strings.TrimSuffix(filename, path.Ext(filename)),
		"{ext}", safeSegment(ext),
		"{subject}", safeSegment(item.Subject),
		"{from}", safeSegment(senderAddress(item.FromAddr)),
		"{message_id}", fmt.Sprintf("%d", item.MessageID),
	}
	out := strings.NewReplacer(replacements...).Replace(template)
	// A template whose last segment resolved to nothing would name a
	// collection rather than a file, so the filename backstops it.
	cleaned := cleanRemotePath(out)
	if cleaned == "" || strings.HasSuffix(cleaned, "/") {
		cleaned = strings.TrimSuffix(cleaned, "/")
		if cleaned != "" {
			cleaned += "/"
		}
		cleaned += filename
	}
	return cleaned
}

// safeSegment reduces one substituted value to a path segment: no separators,
// no relative-path meaning, no control characters, and short enough that a long
// subject line does not exceed what a filesystem behind the WebDAV server will
// take.
func safeSegment(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '?' || r == '*' || r == '"' ||
			r == '<' || r == '>' || r == '|':
			b.WriteRune('-')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	// Runs of dots collapse to one. A `..` left inside a name is not a
	// traversal -- this is one segment, and cleanRemotePath has already
	// resolved the path -- but `..-..-etc-passwd` as a filename reads like
	// something went wrong, and a name nobody has to squint at is worth the
	// one extra pass.
	out := b.String()
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	out = strings.TrimSpace(strings.Trim(strings.TrimSpace(out), "."))
	return strings.TrimSpace(truncateRunes(out, maxSegmentRunes))
}

// maxSegmentRunes bounds one path segment. It is counted in characters rather
// than bytes because the byte budget behind it -- 255 on most filesystems --
// is what a segment has to fit, and 120 characters is inside it even at four
// bytes each.
const maxSegmentRunes = 120

// truncateRunes cuts a string to a character count without splitting a
// character in half. A byte slice would: `out[:120]` through a subject line
// written in German or Japanese lands mid-rune, and the invalid UTF-8 that
// results is written into a Postgres `text` column before the upload is even
// attempted -- which the server refuses, so the row retries its way to
// abandoned and the attachment is never filed.
func truncateRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	count := 0
	for index := range value {
		if count == limit {
			return value[:index]
		}
		count++
	}
	return value
}

// senderAddress reduces a From header to the address inside it, which is what
// reads usefully in a path.
func senderAddress(value string) string {
	value = strings.TrimSpace(value)
	if start := strings.LastIndex(value, "<"); start >= 0 {
		if end := strings.Index(value[start:], ">"); end > 0 {
			return strings.TrimSpace(value[start+1 : start+end])
		}
	}
	return value
}
