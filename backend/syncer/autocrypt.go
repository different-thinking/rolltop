// File overview: Backend plugin hook dispatch during message sync.

package syncer

import (
	"context"
	"errors"
	"log"

	"rolltop/backend/plugins"
	"rolltop/backend/search"
	"rolltop/backend/store"
)

// guardPluginHook runs one plugin hook so a misbehaving plugin can neither crash
// the sync process nor abort the mail-sync operation that invoked it. A panic is
// contained and swallowed by returning nil, so dispatch continues as if the
// plugin learned nothing from this message. Only the panic value's type is
// logged, never the value itself: a custom panic value can carry message-derived
// data, and the same rule the caller sites follow (logging error_type, not the
// error string) applies here. A returned error is handed back for the caller to
// log with its own user/message context. These hooks are advisory (peer-metadata
// discovery, stored-message classification), so degrading to "this plugin did
// nothing" is always preferable to a failed import or a downed process.
func guardPluginHook(pluginID, phase string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("plugin hook panicked plugin_id=%s phase=%s panic_type=%T", pluginID, phase, r)
			err = nil
		}
	}()
	return fn()
}

func (s *Service) importIncomingMessageHooks(ctx context.Context, userID int64, raw []byte, parsedFrom string, junk bool) error {
	// Incoming-message hooks discover peer metadata -- today, Autocrypt public
	// keys -- from the message itself. A spam folder is exactly where a spoofed
	// From with an attacker key lands, so nothing filed as Junk is allowed to
	// teach the account a key. Skipping dispatch entirely keeps that trust
	// decision in one place rather than in each hook.
	if junk {
		return nil
	}
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		return err
	}
	host := syncPluginHost{s: s}
	for _, backendPlugin := range backendPlugins {
		hook, ok := backendPlugin.(plugins.IncomingMessageHook)
		if !ok {
			continue
		}
		generationRecoveryPhase(ctx, "plugin-incoming-message", backendPlugin.ID())
		// Advisory: peer-metadata discovery must never fail a message import or
		// crash the process, so a hook error or panic is logged and dispatch
		// moves on to the next plugin. Context cancellation is the exception --
		// it means the turn itself is being torn down, not that this plugin
		// failed -- so it propagates rather than being masked as an import that
		// quietly succeeded during shutdown.
		if err := guardPluginHook(backendPlugin.ID(), "incoming-message", func() error {
			return hook.ImportIncomingMessage(ctx, host, userID, raw, parsedFrom)
		}); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if !errors.Is(err, plugins.ErrUnsupported) {
				log.Printf("incoming-message hook failed plugin_id=%s user_id=%d error_type=%T", backendPlugin.ID(), userID, err)
			}
		}
	}
	return nil
}

func (s *Service) importStoredMessageHooks(ctx context.Context, hooks []plugins.StoredMessageHook, msg store.MessageRecord, mailbox store.Mailbox) error {
	if len(hooks) == 0 {
		return nil
	}
	host := syncPluginHost{s: s}
	info := plugins.StoredMessageContext{
		UserID:      msg.UserID,
		MessageID:   msg.ID,
		AccountID:   msg.AccountID,
		MailboxID:   msg.MailboxID,
		MailboxName: mailbox.Name,
		UID:         msg.UID,
		Date:        msg.Date,
		From:        msg.FromAddr,
		To:          msg.ToAddr,
		CC:          msg.CCAddr,
		Subject:     msg.Subject,
		IsRead:      msg.IsRead,
		IsStarred:   msg.IsStarred,
	}
	for _, hook := range hooks {
		generationRecoveryPhase(ctx, "plugin-stored-message", hook.ID())
		// Advisory, like the incoming-message hooks and the classifiers: the row
		// is already stored, so a hook error or panic is logged and skipped rather
		// than turned into a failed sync. Context cancellation still propagates --
		// that is the turn ending, not a plugin failure.
		if err := guardPluginHook(hook.ID(), "stored-message", func() error {
			return hook.ImportStoredMessage(ctx, host, info)
		}); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if !errors.Is(err, plugins.ErrUnsupported) {
				log.Printf("stored-message hook failed plugin_id=%s user_id=%d message_id=%d error_type=%T", hook.ID(), info.UserID, info.MessageID, err)
			}
		}
	}
	return nil
}

func (s *Service) storedMessageHooks(ctx context.Context) ([]plugins.StoredMessageHook, error) {
	backendPlugins, err := s.enabledBackendPlugins(ctx)
	if err != nil {
		return nil, err
	}
	hooks := make([]plugins.StoredMessageHook, 0, len(backendPlugins))
	for _, backendPlugin := range backendPlugins {
		hook, ok := backendPlugin.(plugins.StoredMessageHook)
		if ok {
			hooks = append(hooks, hook)
		}
	}
	return hooks, nil
}

type syncPluginHost struct {
	s *Service
}

func (h syncPluginHost) Store() any {
	return h.s.Store
}

func (h syncPluginHost) MasterKey() []byte {
	return h.s.MasterKey
}

func (h syncPluginHost) PluginEnabled(ctx context.Context, pluginID string) bool {
	return h.s.pluginEnabled(ctx, pluginID)
}

func (h syncPluginHost) MatchMessageSearch(ctx context.Context, userID, messageID int64, query string) (plugins.SearchMatchResult, error) {
	if h.s == nil || h.s.Search == nil {
		return plugins.SearchMatchResult{}, errors.New("search is not configured")
	}
	hit, ok, err := h.s.Search.MatchMessageWithOptions(ctx, userID, messageID, query, search.SearchOptions{})
	if err != nil {
		return plugins.SearchMatchResult{}, err
	}
	return plugins.SearchMatchResult{
		Matched:    ok,
		Score:      hit.Score,
		Terms:      hit.Terms,
		QueryTerms: hit.QueryTerms,
		Fields:     hit.Fields,
	}, nil
}

func (h syncPluginHost) SimilarMessages(ctx context.Context, userID int64, request plugins.SimilarMessagesRequest) ([]plugins.SimilarMessageResult, error) {
	if h.s == nil || h.s.Store == nil || h.s.Search == nil {
		return nil, errors.New("message similarity is not configured")
	}
	return h.s.Search.SimilarMessages(ctx, h.s.Store, userID, request)
}

func (h syncPluginHost) StarMessage(ctx context.Context, userID, messageID int64, starred bool) error {
	if h.s == nil {
		return errors.New("sync service is not configured")
	}
	msg, err := h.s.SetStarredForMessage(ctx, userID, messageID, starred)
	if err != nil {
		return err
	}
	return h.s.SyncStarStateForMessage(ctx, userID, msg.ID)
}

// MarkMessageRead sets read state locally and pushes `\Seen`, so a filter that
// marks mail read leaves it in the same state opening it would have.
func (h syncPluginHost) MarkMessageRead(ctx context.Context, userID, messageID int64, read bool) error {
	if h.s == nil {
		return errors.New("sync service is not configured")
	}
	msg, err := h.s.SetReadForMessage(ctx, userID, messageID, read)
	if err != nil {
		return err
	}
	return h.s.SyncReadStateForMessage(ctx, userID, msg.ID)
}

func (h syncPluginHost) MoveMessage(ctx context.Context, userID, messageID, destMailboxID int64) error {
	if h.s == nil {
		return errors.New("sync service is not configured")
	}
	return h.s.MoveMessage(ctx, userID, messageID, destMailboxID)
}

func (h syncPluginHost) ForwardMessage(ctx context.Context, userID, messageID int64, to string, headers []plugins.MailHeader) error {
	if h.s == nil {
		return errors.New("sync service is not configured")
	}
	return h.s.ForwardMessage(ctx, userID, messageID, to, headers)
}

// ArchiveMailboxID answers with the account's chosen Archive folder, or zero
// when the reader has not named one; archiving has no role to fall back on.
func (h syncPluginHost) ArchiveMailboxID(ctx context.Context, userID, accountID int64) (int64, error) {
	if h.s == nil || h.s.Store == nil {
		return 0, errors.New("message store is not configured")
	}
	targets, err := h.s.Store.ArchiveMailboxesForUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	for _, target := range targets {
		if target.AccountID == accountID {
			return target.MailboxID, nil
		}
	}
	return 0, nil
}

var _ plugins.BackendHost = syncPluginHost{}
var _ plugins.StoredMessageHost = syncPluginHost{}
var _ plugins.MessageSimilarityHost = syncPluginHost{}
var _ plugins.MessageClassificationHost = syncPluginHost{}
