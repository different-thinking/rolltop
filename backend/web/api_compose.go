// File overview: Compose, reply, forward, attachment upload, and send API handlers.

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/smtpclient"
	"rolltop/backend/store"
	"rolltop/backend/syncer"
)

const (
	composeMaxUploadBytes  int64 = 80 << 20
	composeMaxRequestBytes int64 = 96 << 20
	// A send waits this long for background work to give up the tenant's
	// turn, and it has to answer before the proxy in front of a hosted instance
	// gives up on the request: past that, the browser gets a bare 502 instead of
	// the sentence naming what the send was waiting on.
	composeForegroundReservationWait = 20 * time.Second
	// A send that takes longer than this is logged with its steps even when it
	// succeeds, so a slow path can be told apart from a failing one.
	composeSlowSendThreshold = 10 * time.Second
	// The archive that follows a Send and archive runs on its own clock. The
	// send has already succeeded, so this move may neither inherit the
	// request's cancellation -- a proxy timing out must not kill the move
	// mid-flight -- nor hold the response long enough to look like a failed
	// send that the user would then retry into a duplicate.
	archiveAfterSendTimeout         = 10 * time.Second
	archiveAfterSendReservationWait = 2 * time.Second
	// The old draft supersedeDraft retires runs on the same clock, for the same
	// reason: the replacement draft is already saved by the time it starts, so a
	// client that navigates away right after saving must not cancel a move that
	// only cleans up the copy left behind.
	draftSupersedeTimeout = 10 * time.Second
)

func (s *Server) apiCompose(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		form, err := s.composeFormForRequest(r)
		if err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		identities := s.composeIdentities(r.Context(), cu)
		writeJSON(w, map[string]any{"compose": form, "compose_from": s.composeFromLabel(r.Context(), cu), "from_identities": identities})
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		form, ok := decodeComposePost(w, r)
		if !ok {
			return
		}
		progress := newComposeSendProgress()
		sent, err := s.sendComposeTracked(r.Context(), cu, form, progress)
		var incomplete *composeSentIncompleteError
		if err != nil && !errors.As(err, &incomplete) {
			// A send failure otherwise exists only in the browser's console: the
			// error text already names the SMTP host and the underlying network or
			// auth failure, which is exactly what is needed to tell a one-off blip
			// from a host that is unreachable every time, but only if it is written
			// down somewhere a user cannot lose by refreshing the page.
			//
			// It is logged whatever the error is. A send the proxy gave up on ends
			// here with a cancelled context, which the generic handler log drops
			// as an ordinary closed tab -- and that is how a send answered with
			// 502 used to leave no line at all, with nothing to say which step it
			// was stuck in.
			log.Printf("send compose failed user_id=%d client_gone=%t %s: %v",
				cu.User.ID, r.Context().Err() != nil, progress.summary(), err)
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		if progress.elapsed() >= composeSlowSendThreshold {
			log.Printf("send compose slow user_id=%d %s", cu.User.ID, progress.summary())
		}
		archived := s.archiveRepliedMessage(r.Context(), cu.User.ID, form)
		s.notifyUserChanged(cu.User.ID)
		if incomplete != nil {
			// SMTP already accepted the message: the mail is gone, whatever went
			// wrong after that. Answering this like a failed request would invite
			// the client to retry sendCompose and hand the recipient a duplicate,
			// so the response reports success and only warns about the local copy.
			log.Printf("send compose incomplete after SMTP accepted user_id=%d %s: %v", cu.User.ID, progress.summary(), incomplete.err)
			writeJSON(w, map[string]any{"ok": true, "sent": true, "warning": incomplete.Error(), "archived_mailbox": archived})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message_id": sent.ID, "archived_mailbox": archived})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) apiComposeDraft(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	form, ok := decodeComposePost(w, r)
	if !ok {
		return
	}
	draft, err := s.saveComposeDraft(r.Context(), cu, form)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.notifyUserChanged(cu.User.ID)
	writeJSON(w, map[string]any{"ok": true, "message_id": draft.ID})
}

func decodeComposePost(w http.ResponseWriter, r *http.Request) (composeForm, bool) {
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		return decodeComposeMultipart(w, r)
	}
	var form composeForm
	if !decodeJSON(w, r, &form) {
		return composeForm{}, false
	}
	return form, true
}

func decodeComposeMultipart(w http.ResponseWriter, r *http.Request) (composeForm, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, composeMaxRequestBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "Attachment upload is too large.")
		return composeForm{}, false
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	payload := strings.TrimSpace(r.FormValue("payload"))
	if payload == "" {
		writeAPIError(w, http.StatusBadRequest, "Compose payload is missing.")
		return composeForm{}, false
	}
	var form composeForm
	if err := json.Unmarshal([]byte(payload), &form); err != nil {
		writeAPIError(w, http.StatusBadRequest, "Compose payload is invalid.")
		return composeForm{}, false
	}
	var total int64
	for i := range form.Attachments {
		meta := &form.Attachments[i]
		meta.Field = strings.TrimSpace(meta.Field)
		if meta.Field == "" {
			meta.Field = fmt.Sprintf("attachment_%d", i)
		}
		files := r.MultipartForm.File[meta.Field]
		if len(files) == 0 {
			writeAPIError(w, http.StatusBadRequest, "Attachment upload is missing.")
			return composeForm{}, false
		}
		file, err := files[0].Open()
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "Attachment upload could not be read.")
			return composeForm{}, false
		}
		remaining := composeMaxUploadBytes - total
		data, readErr := io.ReadAll(io.LimitReader(file, remaining+1))
		_ = file.Close()
		if readErr != nil {
			writeAPIError(w, http.StatusBadRequest, "Attachment upload could not be read.")
			return composeForm{}, false
		}
		if int64(len(data)) > remaining {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "Attachment upload is too large.")
			return composeForm{}, false
		}
		total += int64(len(data))
		meta.Data = data
		meta.Size = int64(len(data))
		if strings.TrimSpace(meta.Filename) == "" {
			meta.Filename = files[0].Filename
		}
		if strings.TrimSpace(meta.Filename) == "" {
			meta.Filename = "attachment"
		}
		meta.ContentType = normalizeUploadContentType(meta.ContentType, files[0].Header.Get("Content-Type"), data)
		if meta.Inline && strings.TrimSpace(meta.ContentID) == "" {
			meta.ContentID = fmt.Sprintf("rolltop-inline-%d", i)
		}
	}
	return form, true
}

func normalizeUploadContentType(metaType, headerType string, data []byte) string {
	contentType := strings.TrimSpace(metaType)
	if contentType == "" {
		contentType = strings.TrimSpace(headerType)
	}
	if contentType == "" || strings.EqualFold(contentType, "application/octet-stream") {
		contentType = http.DetectContentType(data)
	}
	return contentType
}

func composeSMTPAttachments(items []composeAttachment) []smtpclient.Attachment {
	attachments := make([]smtpclient.Attachment, 0, len(items))
	for _, item := range items {
		attachments = append(attachments, smtpclient.Attachment{
			Filename:    item.Filename,
			ContentType: item.ContentType,
			ContentID:   item.ContentID,
			Inline:      item.Inline,
			Data:        item.Data,
		})
	}
	return attachments
}

func composeUploadedAttachmentBytes(items []composeAttachment) int64 {
	var total int64
	for _, item := range items {
		if item.Size > 0 {
			total += item.Size
			continue
		}
		total += int64(len(item.Data))
	}
	return total
}

func composeExistingAttachmentIDs(items []composeExistingAttachment) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		if item.ID > 0 {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

func (s *Server) composeExistingAttachmentsForMessage(ctx context.Context, userID, messageID int64) ([]composeExistingAttachment, error) {
	attachments, err := s.store.ListAttachmentsForMessage(ctx, userID, messageID)
	if err != nil {
		return nil, err
	}
	attachments = visibleAttachments(attachments)
	out := make([]composeExistingAttachment, 0, len(attachments))
	for _, att := range attachments {
		out = append(out, composeExistingAttachment{
			ID:          att.ID,
			Filename:    attachmentDisplayName(att),
			ContentType: att.ContentType,
			Size:        att.Size,
			DownloadURL: fmt.Sprintf("/attachments/%d/download", att.ID),
		})
	}
	return out, nil
}

func (s *Server) composeSMTPExistingAttachments(ctx context.Context, userID int64, ids []int64, remaining int64) ([]smtpclient.Attachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if remaining <= 0 {
		return nil, fmt.Errorf("attachments exceed compose limit")
	}
	seen := map[int64]bool{}
	attachments := make([]smtpclient.Attachment, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		att, err := s.store.GetAttachmentForUser(ctx, userID, id)
		if err != nil {
			return nil, err
		}
		if !isDisplayAttachment(att) {
			continue
		}
		if remaining <= 0 {
			return nil, fmt.Errorf("attachments exceed compose limit")
		}
		data, contentType, err := s.attachmentContentBytes(ctx, userID, att, remaining)
		if err != nil {
			return nil, fmt.Errorf("load attachment %s: %w", attachmentDisplayName(att), err)
		}
		remaining -= int64(len(data))
		if remaining < 0 {
			return nil, fmt.Errorf("attachments exceed compose limit")
		}
		filename := attachmentDisplayName(att)
		if filename == "" {
			filename = "attachment"
		}
		contentType = strings.TrimSpace(contentType)
		if contentType == "" {
			contentType = strings.TrimSpace(att.ContentType)
		}
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		attachments = append(attachments, smtpclient.Attachment{
			Filename:    filename,
			ContentType: contentType,
			Inline:      false,
			Data:        data,
		})
	}
	return attachments, nil
}

func (s *Server) composeMessageAttachments(ctx context.Context, userID int64, form composeForm) ([]smtpclient.Attachment, error) {
	uploadedAttachments := composeSMTPAttachments(form.Attachments)
	remaining := composeMaxUploadBytes - composeUploadedAttachmentBytes(form.Attachments)
	existingAttachments, err := s.composeSMTPExistingAttachments(ctx, userID, form.IncludeAttachmentIDs, remaining)
	if err != nil {
		return nil, err
	}
	remaining -= smtpAttachmentBytes(existingAttachments)
	forwardAttachment, err := s.composeForwardMessageAttachment(ctx, userID, form.ForwardAttachmentID, remaining)
	if err != nil {
		return nil, err
	}
	attachments := append(uploadedAttachments, existingAttachments...)
	if forwardAttachment != nil {
		attachments = append(attachments, *forwardAttachment)
	}
	return attachments, nil
}

func smtpAttachmentBytes(items []smtpclient.Attachment) int64 {
	var total int64
	for _, item := range items {
		total += int64(len(item.Data))
	}
	return total
}

func (s *Server) composeFormForRequest(r *http.Request) (composeForm, error) {
	if raw := strings.TrimSpace(r.URL.Query().Get("draft")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return composeForm{}, store.ErrNotFound
		}
		cu, _ := current(r)
		msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
		if err != nil {
			return composeForm{}, err
		}
		mailbox, err := s.store.GetMailboxForUser(r.Context(), cu.User.ID, msg.MailboxID)
		if err != nil {
			return composeForm{}, err
		}
		if mailbox.Role != "drafts" {
			return composeForm{}, store.ErrNotFound
		}
		form := s.draftComposeFormForMessage(r.Context(), cu, msg)
		attachments, err := s.composeExistingAttachmentsForMessage(r.Context(), cu.User.ID, msg.ID)
		if err != nil {
			return composeForm{}, err
		}
		form.AvailableAttachments = attachments
		form.IncludeAttachmentIDs = composeExistingAttachmentIDs(attachments)
		return form, nil
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("reply_all")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return composeForm{}, store.ErrNotFound
		}
		cu, _ := current(r)
		msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
		if err != nil {
			return composeForm{}, err
		}
		thread, err := s.store.ListThreadMessagesForUser(r.Context(), cu.User.ID, msg)
		if err != nil {
			return composeForm{}, err
		}
		form := s.replyAllComposeFormForMessage(r.Context(), cu, msg, thread, s.ownAddresses(r.Context(), cu.User))
		s.applyReplyComposeDefaults(r.Context(), cu, msg, thread, &form)
		attachments, err := s.composeExistingAttachmentsForMessage(r.Context(), cu.User.ID, msg.ID)
		if err != nil {
			return composeForm{}, err
		}
		form.AvailableAttachments = attachments
		return form, nil
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("reply")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return composeForm{}, store.ErrNotFound
		}
		cu, _ := current(r)
		msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
		if err != nil {
			return composeForm{}, err
		}
		thread, err := s.store.ListThreadMessagesForUser(r.Context(), cu.User.ID, msg)
		if err != nil {
			return composeForm{}, err
		}
		form := s.replyComposeFormForMessage(r.Context(), cu, msg, thread, s.ownAddresses(r.Context(), cu.User))
		s.applyReplyComposeDefaults(r.Context(), cu, msg, thread, &form)
		attachments, err := s.composeExistingAttachmentsForMessage(r.Context(), cu.User.ID, msg.ID)
		if err != nil {
			return composeForm{}, err
		}
		form.AvailableAttachments = attachments
		return form, nil
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("forward")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return composeForm{}, store.ErrNotFound
		}
		cu, _ := current(r)
		msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
		if err != nil {
			return composeForm{}, err
		}
		form := s.forwardComposeFormForMessage(r.Context(), cu.User.ID, msg)
		attachments, err := s.composeExistingAttachmentsForMessage(r.Context(), cu.User.ID, msg.ID)
		if err != nil {
			return composeForm{}, err
		}
		form.AvailableAttachments = attachments
		form.IncludeAttachmentIDs = composeExistingAttachmentIDs(attachments)
		return form, nil
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("forward_attachment")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return composeForm{}, store.ErrNotFound
		}
		cu, _ := current(r)
		msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, id)
		if err != nil {
			return composeForm{}, err
		}
		return s.forwardAsAttachmentComposeForm(msg), nil
	}
	return composeForm{
		To:      strings.TrimSpace(r.URL.Query().Get("to")),
		Cc:      strings.TrimSpace(r.URL.Query().Get("cc")),
		Bcc:     strings.TrimSpace(r.URL.Query().Get("bcc")),
		Subject: strings.TrimSpace(r.URL.Query().Get("subject")),
		Body:    r.URL.Query().Get("body"),
	}, nil
}

func (s *Server) sendCompose(ctx context.Context, cu currentUser, form composeForm) (store.MessageRecord, error) {
	return s.sendComposeTracked(ctx, cu, form, nil)
}

// sendComposeTracked is sendCompose reporting which step it is in, so a send
// that fails or drags can be logged with where its time went. A nil progress
// tracks nothing.
func (s *Server) sendComposeTracked(ctx context.Context, cu currentUser, form composeForm, progress *composeSendProgress) (store.MessageRecord, error) {
	progress.enter("prepare")
	if s.sender == nil {
		return store.MessageRecord{}, errors.New("SMTP sending is not configured")
	}
	identity, err := s.selectedComposeIdentity(ctx, cu, form.FromIdentityID)
	if err != nil {
		return store.MessageRecord{}, err
	}
	smtpAccount, err := s.smtpAccountForIdentity(ctx, cu.User.ID, identity)
	if err != nil {
		return store.MessageRecord{}, err
	}
	imapAccount, sentMailbox, err := s.sentMailboxForIdentity(ctx, cu.User.ID, identity, smtpAccount)
	if err != nil {
		return store.MessageRecord{}, err
	}
	attachments, err := s.composeMessageAttachments(ctx, cu.User.ID, form)
	if err != nil {
		return store.MessageRecord{}, err
	}
	if form.SecurityEncrypted || form.SecuritySigned {
		if len(attachments) > 0 || form.AttachPublicKey {
			return store.MessageRecord{}, errors.New("message security does not support attachments yet")
		}
	} else if form.AttachPublicKey {
		attachment, err := s.composePublicKeyAttachment(ctx, cu.User.ID, identity)
		if err != nil {
			return store.MessageRecord{}, err
		}
		attachments = append(attachments, attachment)
	}
	bodyHTML, bodyText := form.BodyHTML, form.Body
	if !form.SecurityEncrypted && !form.SecuritySigned {
		bodyHTML, bodyText = appendIdentitySignature(form.BodyHTML, form.Body, identity.Signature)
	}
	msg := smtpclient.Message{
		From:        identity.Header,
		To:          []string{form.To},
		Cc:          []string{form.Cc},
		Bcc:         []string{form.Bcc},
		Subject:     form.Subject,
		BodyText:    bodyText,
		BodyHTML:    bodyHTML,
		MessageID:   smtpclient.NewMessageID(identity.Email),
		Date:        time.Now(),
		Attachments: attachments,
	}
	if err := s.applyPluginMIMEBodyOverride(ctx, cu.User.ID, &msg, identity, form); err != nil {
		return store.MessageRecord{}, err
	}
	s.applyPluginMailHeaders(ctx, cu.User.ID, &msg, identity)
	if form.InReplyToID > 0 {
		reply, err := s.store.GetMessageForUser(ctx, cu.User.ID, form.InReplyToID)
		if err != nil && !store.IsNotFound(err) {
			return store.MessageRecord{}, err
		}
		if err == nil {
			msg.InReplyTo = reply.MessageIDHeader
			msg.References = referencesForReply(reply)
		}
	}
	progress.enter("wait_for_background_work")
	finishForeground, err := s.beginComposeForegroundOperation(ctx, cu.User.ID)
	if err != nil {
		return store.MessageRecord{}, fmt.Errorf("message was not sent because delivery could not be scheduled safely%s: %w",
			s.foregroundBlockersSuffix(cu.User.ID), err)
	}
	defer finishForeground()
	// Before the send, not after: the provider files its own copy the moment it
	// accepts the message, and a sync that reaches that copy first would mirror
	// it as ordinary incoming mail. An id recorded for a send that then fails
	// costs nothing, because no message will ever carry it.
	if err := s.store.RecordOutgoingMessageID(ctx, cu.User.ID, imapAccount.ID, msg.MessageID); err != nil {
		return store.MessageRecord{}, err
	}
	progress.enter("smtp")
	raw, err := s.sender.Send(ctx, smtpEnvelopeForIdentity(identity, smtpAccount), msg)
	if err != nil {
		return store.MessageRecord{}, err
	}
	// The message has left through SMTP. Every error from here on describes a
	// local bookkeeping failure, not a failed send, and must not be reported as
	// one: the caller answers a request that already delivered the mail.
	progress.enter("append_to_sent")
	fetched, err := s.appendSentMessage(ctx, imapAccount, sentMailbox, raw, msg.MessageID, msg.Date)
	if err != nil {
		return store.MessageRecord{}, &composeSentIncompleteError{
			err: fmt.Errorf("message sent through SMTP, but could not save it to %s: %w", sentMailbox.Name, err),
		}
	}
	progress.enter("store_locally")
	sentMsg, err := s.storeSentMessage(ctx, cu.User.ID, imapAccount, sentMailbox, msg, form, fetched)
	if err != nil {
		return store.MessageRecord{}, &composeSentIncompleteError{
			err: fmt.Errorf("message sent through SMTP and saved to %s, but could not be recorded locally: %w", sentMailbox.Name, err),
		}
	}
	progress.enter("done")
	return sentMsg, nil
}

// composeSendProgress records the steps of one send and when each began. It
// exists for the log line alone: a send that hangs is cut off by the proxy in
// front of the app, and the step it was in is the only thing that says whether
// it was waiting on background work, on the SMTP server, or on the IMAP copy.
type composeSendProgress struct {
	start time.Time
	steps []composeSendStep
}

type composeSendStep struct {
	name  string
	began time.Time
}

func newComposeSendProgress() *composeSendProgress {
	return &composeSendProgress{start: time.Now()}
}

func (p *composeSendProgress) enter(step string) {
	if p == nil {
		return
	}
	p.steps = append(p.steps, composeSendStep{name: step, began: time.Now()})
}

func (p *composeSendProgress) elapsed() time.Duration {
	if p == nil {
		return 0
	}
	return time.Since(p.start)
}

// summary names the step the send ended in and how long each step took, e.g.
// "step=smtp elapsed=31s steps=prepare:40ms,wait_for_background_work:2ms,smtp:31s".
func (p *composeSendProgress) summary() string {
	if p == nil {
		return ""
	}
	now := time.Now()
	last := "none"
	parts := make([]string, 0, len(p.steps))
	for i, step := range p.steps {
		end := now
		if i+1 < len(p.steps) {
			end = p.steps[i+1].began
		}
		last = step.name
		if step.name == "done" {
			continue
		}
		parts = append(parts, step.name+":"+end.Sub(step.began).Round(time.Millisecond).String())
	}
	return fmt.Sprintf("step=%s elapsed=%s steps=%s", last, now.Sub(p.start).Round(time.Millisecond), strings.Join(parts, ","))
}

// foregroundBlockersSuffix names the background work a send gave up waiting
// for, in the words the Activity view uses, so the reader is told what to wait
// out or stop there instead of only that something was in the way.
func (s *Server) foregroundBlockersSuffix(userID int64) string {
	if s.syncRunner == nil {
		return ""
	}
	var names []string
	seen := map[string]bool{}
	for _, activity := range s.syncRunner.WorkerActivities(userID) {
		if activity.Waiting {
			continue
		}
		name := syncer.WorkerKindLabel(activity.Kind)
		if activity.Mailbox != "" {
			name += " (" + activity.Mailbox + ")"
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	return " (still running: " + strings.Join(names, ", ") + ")"
}

// composeSentIncompleteError wraps a failure that happened only after SMTP
// already accepted the message for delivery. The distinction matters to every
// caller: reporting this the same way as a send failure would let a retry
// resend a message that already reached its recipient.
type composeSentIncompleteError struct {
	err error
}

func (e *composeSentIncompleteError) Error() string { return e.err.Error() }
func (e *composeSentIncompleteError) Unwrap() error { return e.err }

// archiveRepliedMessage files the message a reply answered, and names the folder
// it landed in so the browser can say so. It runs after the send, never before:
// the reply is already gone, and a message that failed to move is still in the
// list where the reader left it.
//
// Nothing here can fail the request. Sending is the part the user cannot redo,
// and reporting a delivered message as a failure because a folder move went
// wrong would be a worse answer than the empty string this returns.
func (s *Server) archiveRepliedMessage(ctx context.Context, userID int64, form composeForm) string {
	if !form.ArchiveAfterSend || form.InReplyToID <= 0 || s.syncer == nil {
		return ""
	}
	// Detach from the request's lifetime, then bound the work independently.
	// The message is already sent: a client that disconnects must not abort
	// the move halfway, and a move that cannot finish promptly is given up on
	// rather than delaying the answer to a send that succeeded.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), archiveAfterSendTimeout)
	defer cancel()
	message, err := s.store.GetMessageForUser(ctx, userID, form.InReplyToID)
	if err != nil {
		if !store.IsNotFound(err) {
			log.Printf("archive after send user_id=%d message_id=%d: %v", userID, form.InReplyToID, err)
		}
		return ""
	}
	choices, err := s.store.ArchiveMailboxesForUser(ctx, userID)
	if err != nil {
		log.Printf("archive after send user_id=%d: %v", userID, err)
		return ""
	}
	destID := int64(0)
	for _, choice := range choices {
		if choice.AccountID == message.AccountID {
			destID = choice.MailboxID
			break
		}
	}
	// No Archive folder chosen for this account, or the message is already in
	// it. Both are ordinary states, not failures: the reply went out either way.
	if destID <= 0 || destID == message.MailboxID {
		return ""
	}
	dest, err := s.store.GetMailboxForUser(ctx, userID, destID)
	if err != nil {
		log.Printf("archive after send user_id=%d mailbox_id=%d: %v", userID, destID, err)
		return ""
	}
	refreshMailboxes, err := s.moveRefreshMailboxNames(ctx, userID, []int64{message.ID}, dest)
	if err != nil {
		log.Printf("archive after send user_id=%d message_id=%d: %v", userID, message.ID, err)
		return ""
	}
	finishForeground, err := s.beginComposeForegroundOperationWithin(ctx, userID, archiveAfterSendReservationWait)
	if err != nil {
		log.Printf("archive after send user_id=%d message_id=%d: %v", userID, message.ID, err)
		return ""
	}
	defer finishForeground()
	if err := s.syncer.MoveMessage(ctx, userID, message.ID, destID); err != nil {
		log.Printf("archive after send user_id=%d message_id=%d: %v", userID, message.ID, err)
		return ""
	}
	s.startMoveRefresh(userID, dest.AccountID, refreshMailboxes)
	return dest.Name
}

func (s *Server) beginComposeForegroundOperation(ctx context.Context, userID int64) (func(), error) {
	return s.beginComposeForegroundOperationWithin(ctx, userID, composeForegroundReservationWait)
}

func (s *Server) beginComposeForegroundOperationWithin(ctx context.Context, userID int64, wait time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return func() {}, err
	}
	if s.syncRunner == nil {
		return func() {}, nil
	}
	reservationCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return s.syncRunner.BeginForegroundOperation(reservationCtx, userID)
}

func (s *Server) composePublicKeyAttachment(ctx context.Context, userID int64, identity composeIdentity) (smtpclient.Attachment, error) {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		return smtpclient.Attachment{}, err
	}
	identityCtx := pluginMailIdentityContext(identity)
	for _, backendPlugin := range backendPlugins {
		provider, ok := backendPlugin.(plugins.IdentityAttachmentProvider)
		if !ok {
			continue
		}
		attachment, attachmentErr := provider.ComposeIdentityAttachment(ctx, s, userID, identityCtx, "public-key")
		if errors.Is(attachmentErr, plugins.ErrUnsupported) {
			continue
		}
		if attachmentErr != nil {
			return smtpclient.Attachment{}, attachmentErr
		}
		return smtpclient.Attachment{
			Filename:    attachment.Filename,
			ContentType: attachment.ContentType,
			Inline:      attachment.Inline,
			Data:        attachment.Data,
		}, nil
	}
	return smtpclient.Attachment{}, errors.New("this identity does not have a public key attachment")
}
