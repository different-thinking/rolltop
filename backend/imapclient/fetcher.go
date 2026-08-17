// File overview: IMAP fetching, mailbox listing, and mailbox watch implementation.

package imapclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"

	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/store"
	"rolltop/backend/syncer"
	"rolltop/backend/xoauth2"
)

const (
	idleCycleDuration         = 29 * time.Minute
	idleStopGrace             = 5 * time.Second
	defaultIMAPCommandTimeout = 60 * time.Second
	defaultFetchBatchSize     = 10
	// defaultFetchBatchBytes bounds the raw bodies one fetch batch may hold.
	// Message count alone does not: ten ordinary mails are a megabyte, ten mails
	// carrying video attachments are gigabytes, and the sync path holds a whole
	// batch until its last message has been stored so the IMAP command deadline
	// cannot expire while messages are being written.
	defaultFetchBatchBytes = 16 * 1024 * 1024
	// uidSizeProbeWindow bounds one metadata probe. It covers many more UIDs than
	// a body batch can hold, because the probe transfers a few dozen bytes per
	// message and one extra command per two hundred is not worth saving. The
	// window also bounds the size map the probe returns, which is why a
	// contiguous run that would encode as a single short range is split anyway.
	uidSizeProbeWindow = 200
	// maxUIDProbeCommandBytes bounds the encoded UID set instead of counting
	// UIDs, because that is what RFC 2683 advises keeping short. A contiguous
	// window collapses into one range and never approaches it; a sparse repair
	// set of seven-digit UIDs would, so it is split until it fits.
	maxUIDProbeCommandBytes = 800
	// assumedMessageBytes stands in for a size the server did not report. It is
	// the batch budget divided by the default batch size, so an unhelpful server
	// simply gets the count-based batching this fetcher has always used.
	assumedMessageBytes = defaultFetchBatchBytes / defaultFetchBatchSize
)

var errIdleStopTimeout = errors.New("IDLE session did not stop cleanly")

// Fetcher implements syncer.Fetcher using go-imap and encrypted Rolltop account credentials.
type Fetcher struct {
	MasterKey []byte
	Timeout   time.Duration
	BatchSize uint32
	// BatchBytes caps the raw bodies one batch may hold. Zero selects
	// defaultFetchBatchBytes; a batch always fetches at least one message, so a
	// message larger than this is fetched on its own rather than skipped.
	BatchBytes int64
	// Tokens mints Google access tokens for accounts that authenticate with
	// OAuth. It is an interface rather than the manager itself so this package
	// stays independent of the OAuth implementation and can be tested without
	// one. A nil value only fails accounts that actually need it.
	Tokens xoauth2.TokenSource
}

// ServerCapabilities contains the authenticated IMAP extensions used by
// copy-only remote synchronization.
type ServerCapabilities struct {
	IDLE    bool
	UIDPlus bool
}

type capabilitySupporter interface {
	Support(string) (bool, error)
}

// ProbeCapabilities authenticates before checking capabilities because an
// IMAP server may advertise a different extension set after LOGIN.
func (f *Fetcher) ProbeCapabilities(ctx context.Context, account store.MailAccount) (ServerCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return ServerCapabilities{}, err
	}
	if f == nil {
		return ServerCapabilities{}, errors.New("probe IMAP capabilities requires a fetcher")
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return ServerCapabilities{}, err
	}
	defer terminateClientOnContext(ctx, c)()
	capabilities, err := probeCapabilities(c)
	if err != nil {
		return ServerCapabilities{}, err
	}
	if err := ctx.Err(); err != nil {
		return ServerCapabilities{}, err
	}
	return capabilities, nil
}

func probeCapabilities(supporter capabilitySupporter) (ServerCapabilities, error) {
	if supporter == nil {
		return ServerCapabilities{}, errors.New("probe IMAP capabilities requires a client")
	}
	idle, err := supporter.Support("IDLE")
	if err != nil {
		return ServerCapabilities{}, fmt.Errorf("check IMAP IDLE support: %w", err)
	}
	uidPlus, err := supporter.Support("UIDPLUS")
	if err != nil {
		return ServerCapabilities{}, fmt.Errorf("check IMAP UIDPLUS support: %w", err)
	}
	return ServerCapabilities{IDLE: idle, UIDPlus: uidPlus}, nil
}

// ListMailboxes logs in, lists selectable folders, and returns only names. It does
// not create local rows; sync.Service decides which folders belong to the user DB.
func (f *Fetcher) ListMailboxes(ctx context.Context, account store.MailAccount) ([]syncer.MailboxInfo, error) {
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return nil, err
	}
	defer terminateClientOnContext(ctx, c)()

	mailboxes := make(chan *imap.MailboxInfo, 50)
	done := make(chan error, 1)
	go func() {
		done <- c.List("", "*", mailboxes)
	}()

	var out []syncer.MailboxInfo
	for mb := range mailboxes {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		info, ok := mailboxDiscoveryInfo(mb)
		if !ok {
			continue
		}
		out = append(out, info)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}
	return out, nil
}

func mailboxDiscoveryInfo(mailbox *imap.MailboxInfo) (syncer.MailboxInfo, bool) {
	if mailbox == nil || hasAttr(mailbox.Attributes, imap.NoSelectAttr) {
		return syncer.MailboxInfo{}, false
	}
	return syncer.MailboxInfo{Name: mailbox.Name, Attributes: append([]string(nil), mailbox.Attributes...)}, true
}

type trainingCandidateClient interface {
	Select(name string, readOnly bool) (*imap.MailboxStatus, error)
	UidSearch(criteria *imap.SearchCriteria) ([]uint32, error)
	UidFetch(seqset *imap.SeqSet, items []imap.FetchItem, ch chan *imap.Message) error
}

// SearchTrainingCandidates performs a read-only UID search and fetches only
// flags, dates, sizes, and envelope fields for the newest bounded matches. It
// does not fetch a message body or write any local/remote state.
func (f *Fetcher) SearchTrainingCandidates(ctx context.Context, account store.MailAccount, mailbox string, query syncer.TrainingCandidateQuery) (syncer.TrainingCandidateSearch, error) {
	if err := ctx.Err(); err != nil {
		return syncer.TrainingCandidateSearch{}, err
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return syncer.TrainingCandidateSearch{}, err
	}
	defer terminateClientOnContext(ctx, c)()
	return f.searchTrainingCandidates(ctx, c, mailbox, query)
}

func (f *Fetcher) searchTrainingCandidates(ctx context.Context, c trainingCandidateClient, mailbox string, query syncer.TrainingCandidateQuery) (syncer.TrainingCandidateSearch, error) {
	mailbox = strings.TrimSpace(mailbox)
	if mailbox == "" {
		return syncer.TrainingCandidateSearch{}, errors.New("search training candidates requires a mailbox name")
	}
	if !query.Since.IsZero() && !query.Before.IsZero() && !query.Before.After(query.Since) {
		return syncer.TrainingCandidateSearch{}, errors.New("training candidate before date must be after since date")
	}
	limit := query.Limit
	if limit <= 0 || limit > syncer.MaxTrainingCandidateCount {
		limit = syncer.MaxTrainingCandidateCount
	}
	selected, err := c.Select(mailbox, true)
	if err != nil {
		return syncer.TrainingCandidateSearch{}, fmt.Errorf("select mailbox %q read-only for training search: %w", mailbox, err)
	}
	if selected == nil || selected.Messages == 0 {
		return syncer.TrainingCandidateSearch{}, nil
	}
	criteria := trainingCandidateSearchCriteria(query)
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return syncer.TrainingCandidateSearch{}, fmt.Errorf("search training candidates in mailbox %q: %w", mailbox, err)
	}
	allUIDs := normalizeUIDList(uids)
	out := syncer.TrainingCandidateSearch{Matched: len(allUIDs)}
	uids = newestTrainingUIDs(allUIDs, limit)
	return f.fetchTrainingMetadata(ctx, c, mailbox, uids, out)
}

func trainingCandidateSearchCriteria(query syncer.TrainingCandidateQuery) *imap.SearchCriteria {
	criteria := imap.NewSearchCriteria()
	criteria.Since = query.Since
	criteria.Before = query.Before
	if query.SeenOnly {
		criteria.WithFlags = append(criteria.WithFlags, imap.SeenFlag)
	}
	return criteria
}

func newestTrainingUIDs(uids []uint32, limit int) []uint32 {
	uids = normalizeUIDList(uids)
	if len(uids) > limit {
		uids = uids[len(uids)-limit:]
	}
	out := append([]uint32(nil), uids...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (f *Fetcher) fetchTrainingMetadata(ctx context.Context, c trainingCandidateClient, mailbox string, uids []uint32, out syncer.TrainingCandidateSearch) (syncer.TrainingCandidateSearch, error) {
	if len(uids) == 0 {
		return out, nil
	}
	items := []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchFlags}
	for i := 0; i < len(uids); i += f.trainingBatchSize() {
		if err := ctx.Err(); err != nil {
			return syncer.TrainingCandidateSearch{}, err
		}
		end := i + f.trainingBatchSize()
		if end > len(uids) {
			end = len(uids)
		}
		requested := uids[i:end]
		seqset := new(imap.SeqSet)
		seqset.AddNum(requested...)
		messages := make(chan *imap.Message, len(requested))
		done := make(chan error, 1)
		go func() {
			done <- c.UidFetch(seqset, items, messages)
		}()
		byUID := make(map[uint32]syncer.TrainingCandidateMetadata, len(requested))
		var batchErr error
		for msg := range messages {
			if msg == nil || msg.Uid == 0 {
				continue
			}
			metadata := syncer.TrainingCandidateMetadata{
				Mailbox:      mailbox,
				UID:          msg.Uid,
				InternalDate: msg.InternalDate,
				Size:         int64(msg.Size),
				Flags:        append([]string(nil), msg.Flags...),
			}
			if msg.Envelope != nil {
				metadata.Date = msg.Envelope.Date
				metadata.Subject = msg.Envelope.Subject
				metadata.From = trainingAddresses(msg.Envelope.From)
				metadata.To = trainingAddresses(msg.Envelope.To)
				metadata.MessageID = msg.Envelope.MessageId
			}
			if _, exists := byUID[msg.Uid]; exists {
				if batchErr == nil {
					batchErr = fmt.Errorf("IMAP server returned training UID %d more than once", msg.Uid)
				}
				continue
			}
			byUID[msg.Uid] = metadata
		}
		if err := <-done; err != nil {
			return syncer.TrainingCandidateSearch{}, fmt.Errorf("fetch training metadata mailbox %q UID batch %s: %w", mailbox, describeBatch(requested), err)
		}
		if batchErr != nil {
			return syncer.TrainingCandidateSearch{}, batchErr
		}
		if err := ctx.Err(); err != nil {
			return syncer.TrainingCandidateSearch{}, err
		}
		for _, uid := range requested {
			metadata, ok := byUID[uid]
			if !ok {
				return syncer.TrainingCandidateSearch{}, fmt.Errorf("IMAP server omitted training metadata mailbox %q UID %d", mailbox, uid)
			}
			out.Candidates = append(out.Candidates, metadata)
		}
	}
	return out, nil
}

func trainingAddresses(addresses []*imap.Address) []string {
	out := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if address == nil {
			continue
		}
		value := strings.TrimSpace(address.Address())
		if value != "" && value != "@" {
			out = append(out, value)
		}
	}
	return out
}

// FetchTrainingCandidates downloads a caller-selected, bounded UID set using a
// 512 KiB BODY.PEEK partial. The mailbox is selected read-only and the payloads
// are delivered only to the callback; this method does not persist them.
func (f *Fetcher) FetchTrainingCandidates(ctx context.Context, account store.MailAccount, mailbox string, uids []uint32, handle func(syncer.TrainingCandidate) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mailbox = strings.TrimSpace(mailbox)
	if mailbox == "" {
		return errors.New("fetch training candidates requires a mailbox name")
	}
	if handle == nil {
		return errors.New("fetch training candidates requires a handler")
	}
	var err error
	uids, err = normalizeTrainingUIDs(uids)
	if err != nil {
		return err
	}
	if len(uids) == 0 {
		return nil
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return err
	}
	defer terminateClientOnContext(ctx, c)()
	return f.fetchTrainingCandidates(ctx, c, mailbox, uids, handle)
}

func (f *Fetcher) fetchTrainingCandidates(ctx context.Context, c trainingCandidateClient, mailbox string, uids []uint32, handle func(syncer.TrainingCandidate) error) error {
	if _, err := c.Select(mailbox, true); err != nil {
		return fmt.Errorf("select mailbox %q read-only for training fetch: %w", mailbox, err)
	}
	section := trainingBodySection()
	items := []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchFlags, section.FetchItem()}
	for i := 0; i < len(uids); i += f.trainingBatchSize() {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := i + f.trainingBatchSize()
		if end > len(uids) {
			end = len(uids)
		}
		requested := uids[i:end]
		seqset := new(imap.SeqSet)
		seqset.AddNum(requested...)
		messages := make(chan *imap.Message, len(requested))
		done := make(chan error, 1)
		go func() {
			done <- c.UidFetch(seqset, items, messages)
		}()
		byUID := make(map[uint32]syncer.TrainingCandidate, len(requested))
		var readErr error
		var batchErr error
		for msg := range messages {
			if msg == nil || msg.Uid == 0 {
				continue
			}
			body := msg.GetBody(section)
			if body == nil {
				continue
			}
			raw, err := io.ReadAll(io.LimitReader(body, syncer.MaxTrainingCandidateBodyBytes+1))
			if err != nil {
				if readErr == nil {
					readErr = fmt.Errorf("read training body mailbox %q UID %d: %w", mailbox, msg.Uid, err)
				}
				continue
			}
			truncated := len(raw) > syncer.MaxTrainingCandidateBodyBytes || int64(msg.Size) > syncer.MaxTrainingCandidateBodyBytes
			if len(raw) > syncer.MaxTrainingCandidateBodyBytes {
				raw = raw[:syncer.MaxTrainingCandidateBodyBytes]
			}
			if _, exists := byUID[msg.Uid]; exists {
				if batchErr == nil {
					batchErr = fmt.Errorf("IMAP server returned training UID %d more than once", msg.Uid)
				}
				continue
			}
			byUID[msg.Uid] = syncer.TrainingCandidate{
				FetchedMessage: syncer.FetchedMessage{
					Mailbox:      mailbox,
					UID:          msg.Uid,
					InternalDate: msg.InternalDate,
					Size:         int64(msg.Size),
					Flags:        append([]string(nil), msg.Flags...),
					Raw:          raw,
				},
				Truncated: truncated,
			}
		}
		if err := <-done; err != nil {
			return fmt.Errorf("fetch training bodies mailbox %q UID batch %s: %w", mailbox, describeBatch(requested), err)
		}
		if batchErr != nil {
			return batchErr
		}
		if readErr != nil {
			return readErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, uid := range requested {
			candidate, ok := byUID[uid]
			if !ok {
				return fmt.Errorf("IMAP server omitted training body mailbox %q UID %d", mailbox, uid)
			}
			if err := handle(candidate); err != nil {
				return fmt.Errorf("handle training candidate mailbox %q UID %d: %w", mailbox, uid, err)
			}
		}
	}
	return nil
}

func normalizeTrainingUIDs(uids []uint32) ([]uint32, error) {
	if len(uids) > syncer.MaxTrainingCandidateCount {
		return nil, fmt.Errorf("training candidate UID count %d exceeds maximum %d", len(uids), syncer.MaxTrainingCandidateCount)
	}
	seen := make(map[uint32]bool, len(uids))
	out := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if uid == 0 {
			return nil, errors.New("training candidate UIDs must be nonzero")
		}
		if !seen[uid] {
			seen[uid] = true
			out = append(out, uid)
		}
	}
	return out, nil
}

func (f *Fetcher) trainingBatchSize() int {
	const maxTrainingBatchSize = 20
	if f != nil && f.BatchSize > 0 && f.BatchSize < maxTrainingBatchSize {
		return int(f.BatchSize)
	}
	if f != nil && f.BatchSize >= maxTrainingBatchSize {
		return maxTrainingBatchSize
	}
	return 10
}

func trainingBodySection() *imap.BodySectionName {
	return &imap.BodySectionName{Peek: true, Partial: []int{0, syncer.MaxTrainingCandidateBodyBytes}}
}

var _ syncer.TrainingCandidateFetcher = (*Fetcher)(nil)

// MailboxStatus uses IMAP STATUS instead of SELECT where possible so folder counts
// and UIDNEXT can be refreshed cheaply for progress planning and UI hints.
func (f *Fetcher) MailboxStatus(ctx context.Context, account store.MailAccount, mailbox string) (syncer.MailboxStatus, error) {
	select {
	case <-ctx.Done():
		return syncer.MailboxStatus{}, ctx.Err()
	default:
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return syncer.MailboxStatus{}, err
	}
	defer terminateClientOnContext(ctx, c)()
	status, err := c.Status(mailbox, []imap.StatusItem{imap.StatusMessages, imap.StatusUnseen, imap.StatusUidNext, imap.StatusUidValidity})
	if err != nil {
		return syncer.MailboxStatus{}, fmt.Errorf("status mailbox %q: %w", mailbox, err)
	}
	return syncer.MailboxStatus{Messages: status.Messages, Unseen: status.Unseen, UIDNext: status.UidNext, UIDValidity: status.UidValidity}, nil
}

// CreateMailbox issues IMAP CREATE for a server-side folder. Rolltop creates the
// local mailbox row only after this succeeds.
func (f *Fetcher) CreateMailbox(ctx context.Context, account store.MailAccount, mailbox string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	mailbox = strings.TrimSpace(mailbox)
	if mailbox == "" {
		return fmt.Errorf("folder name is required")
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return err
	}
	defer terminateClientOnContext(ctx, c)()
	if err := c.Create(mailbox); err != nil {
		return fmt.Errorf("create IMAP folder %q: %w", mailbox, err)
	}
	return nil
}

// UIDs returns every UID currently present in a mailbox for local deletion/reconciliation checks.
func (f *Fetcher) UIDs(ctx context.Context, account store.MailAccount, mailbox string) ([]uint32, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return nil, err
	}
	defer terminateClientOnContext(ctx, c)()
	if _, err := c.Select(mailbox, true); err != nil {
		return nil, fmt.Errorf("select mailbox %q read-only for UID reconcile: %w", mailbox, err)
	}
	criteria := imap.NewSearchCriteria()
	criteria.Uid = new(imap.SeqSet)
	criteria.Uid.AddRange(1, 0)
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search UIDs in mailbox %q: %w", mailbox, err)
	}
	return uids, nil
}

// limitToSyncStart drops messages delivered before the account's configured
// start date from a search. Applying it at the server means a mailbox with
// twenty years of history never sends the bodies at all, which is the whole
// point: the alternative is downloading everything and discarding it here.
//
// SINCE compares INTERNALDATE at day granularity, so the boundary day is
// included whole. Erring towards one extra day beats silently dropping mail
// that arrived on the chosen date.
//
// This belongs only on searches that decide what to download. The searches that
// decide what to delete or how to set flags must keep seeing the whole mailbox:
// reconciliation removes local messages missing from the server's UID list, and
// UpdateMailboxReadFlags marks every local message not in the seen list as
// unread. Handing either a cutoff-limited list would delete or unread the mail
// that was mirrored before the cutoff was set. Repair gets its own limited list
// through MailboxUIDSnapshot.FetchableUIDs instead.
func limitToSyncStart(criteria *imap.SearchCriteria, account store.MailAccount) *imap.SearchCriteria {
	if account.SyncStartAt.IsZero() {
		return criteria
	}
	criteria.Since = account.SyncStartAt
	return criteria
}

// FetchMailbox is the incremental body fetch path. It selects read-only, searches
// for UIDs greater than afterUID, fetches RFC822 bodies in batches, and streams each
// result to the syncer callback instead of accumulating a mailbox in memory.
func (f *Fetcher) FetchMailbox(ctx context.Context, account store.MailAccount, mailbox string, afterUID uint32, handle func(syncer.FetchedMessage) error) error {
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return err
	}
	defer terminateClientOnContext(ctx, c)()

	mbox, err := c.Select(mailbox, true)
	if err != nil {
		return fmt.Errorf("select mailbox %q read-only: %w", mailbox, err)
	}
	if mbox.Messages == 0 || mbox.UidNext <= afterUID+1 || afterUID == ^uint32(0) {
		return nil
	}

	criteria := imap.NewSearchCriteria()
	criteria.Uid = new(imap.SeqSet)
	criteria.Uid.AddRange(afterUID+1, 0)
	uids, err := c.UidSearch(limitToSyncStart(criteria, account))
	if err != nil {
		return fmt.Errorf("search new UIDs in mailbox %q after UID %d: %w", mailbox, afterUID, err)
	}
	return f.fetchUIDs(ctx, c, mailbox, uids, handle)
}

// FetchUIDs fetches a known sparse UID set. Explicit folder repair uses this to
// fill local holes without downloading every already-mirrored message body.
func (f *Fetcher) FetchUIDs(ctx context.Context, account store.MailAccount, mailbox string, uids []uint32, handle func(syncer.FetchedMessage) error) error {
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return err
	}
	defer terminateClientOnContext(ctx, c)()
	if _, err := c.Select(mailbox, true); err != nil {
		return fmt.Errorf("select mailbox %q read-only: %w", mailbox, err)
	}
	return f.fetchUIDs(ctx, c, mailbox, uids, handle)
}

// fetchUIDs downloads bodies in batches that respect both a message count and a
// byte budget. The whole batch stays in memory until its last message has been
// stored, deliberately: handing messages over while the UID FETCH is still open
// would put storage, MIME parsing, and Bleve commits back inside the IMAP
// command deadline. Planning the batch in bytes is what keeps that decision
// affordable, so a folder with large attachments no longer decides how much
// memory a sync turn needs.
func (f *Fetcher) fetchUIDs(ctx context.Context, c *client.Client, mailbox string, uids []uint32, handle func(syncer.FetchedMessage) error) error {
	uids = normalizeUIDList(uids)
	if len(uids) == 0 {
		return nil
	}
	if handle == nil {
		return errors.New("fetch UIDs requires a message handler")
	}
	batchSize := f.batchSize()
	section := rawBodySection()
	items := []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchFlags, section.FetchItem()}
	probeUnavailable := false
	for _, window := range uidProbeWindows(uids, batchSize) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var sizes map[uint32]int64
		if !probeUnavailable {
			probed, err := probeUIDSizes(ctx, c, mailbox, window, batchSize)
			switch {
			case err == nil:
				sizes = probed
			case ctx.Err() != nil:
				return err
			default:
				// A server that will not answer a metadata probe still gets its
				// bodies. Planning by message count is what this fetcher did
				// before it planned by bytes, so the fetch continues rather than
				// failing the whole folder over a command the sync only wanted in
				// order to size its batches.
				probeUnavailable = true
				log.Printf("fetch mailbox %q plans batches by message count: %v", mailbox, err)
			}
		}
		for _, requested := range planUIDFetchBatches(window, sizes, batchSize, f.batchBytes()) {
			if err := f.fetchUIDBatch(ctx, c, mailbox, requested, section, items, handle); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *Fetcher) batchSize() int {
	if f != nil && f.BatchSize > 0 {
		return int(f.BatchSize)
	}
	return defaultFetchBatchSize
}

func (f *Fetcher) batchBytes() int64 {
	if f != nil && f.BatchBytes > 0 {
		return f.BatchBytes
	}
	return defaultFetchBatchBytes
}

// uidProbeWindows splits a UID list into the spans one size probe covers, by UID
// count and by the length of the command that carries them. A fetcher that
// already downloads one message at a time has nothing to plan, so it keeps the
// whole list as a single window and skips the probe entirely.
func uidProbeWindows(uids []uint32, batchSize int) [][]uint32 {
	if batchSize <= 1 || len(uids) <= 1 {
		return [][]uint32{uids}
	}
	windows := make([][]uint32, 0, (len(uids)+uidSizeProbeWindow-1)/uidSizeProbeWindow)
	for start := 0; start < len(uids); {
		end := min(start+uidSizeProbeWindow, len(uids))
		for end-start > 1 && encodedUIDSetBytes(uids[start:end]) > maxUIDProbeCommandBytes {
			end = start + (end-start)/2
		}
		windows = append(windows, uids[start:end])
		start = end
	}
	return windows
}

// encodedUIDSetBytes measures the set as the server will receive it, so ranges
// are counted as ranges rather than as the UIDs they stand for.
func encodedUIDSetBytes(uids []uint32) int {
	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	return len(seqset.String())
}

// probeUIDSizes reads RFC822.SIZE for one window. The size of a message arrives
// with the body it is supposed to bound, so the only way to plan a body fetch by
// bytes is to ask for the sizes first; the answer is a few dozen bytes per
// message. A server that omits a size leaves that message out of the map and
// gets the count-based batching this fetcher has always used.
func probeUIDSizes(ctx context.Context, c *client.Client, mailbox string, uids []uint32, batchSize int) (map[uint32]int64, error) {
	if batchSize <= 1 || len(uids) <= 1 {
		return nil, nil
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	messages := make(chan *imap.Message, 64)
	done := make(chan error, 1)
	go func() {
		done <- guardedUIDFetch(ctx, c, seqset, []imap.FetchItem{imap.FetchUid, imap.FetchRFC822Size}, messages)
	}()
	sizes := make(map[uint32]int64, len(uids))
	for msg := range messages {
		if msg == nil || msg.Uid == 0 || msg.Size == 0 {
			continue
		}
		sizes[msg.Uid] = int64(msg.Size)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch mailbox %q UID size batch %s: %w", mailbox, describeBatch(uids), err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return sizes, nil
}

// planUIDFetchBatches groups UIDs into batches that stay inside both budgets. A
// message larger than the byte budget on its own still gets fetched, in a batch
// of its own: it cannot be made smaller, but it must not arrive alongside
// anything else.
func planUIDFetchBatches(uids []uint32, sizes map[uint32]int64, maxMessages int, maxBytes int64) [][]uint32 {
	if len(uids) == 0 {
		return nil
	}
	if maxMessages < 1 {
		maxMessages = 1
	}
	batches := make([][]uint32, 0, (len(uids)+maxMessages-1)/maxMessages)
	start := 0
	batchBytes := int64(0)
	for i, uid := range uids {
		size := int64(assumedMessageBytes)
		if known, ok := sizes[uid]; ok && known > 0 {
			size = known
		}
		if i > start && (i-start >= maxMessages || (maxBytes > 0 && batchBytes+size > maxBytes)) {
			batches = append(batches, uids[start:i])
			start = i
			batchBytes = 0
		}
		batchBytes += size
	}
	return append(batches, uids[start:])
}

func (f *Fetcher) fetchUIDBatch(ctx context.Context, c *client.Client, mailbox string, requested []uint32,
	section *imap.BodySectionName, items []imap.FetchItem, handle func(syncer.FetchedMessage) error,
) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(requested...)
	// The buffer never needs to outgrow the batch, and sizing it to the batch is
	// what makes the byte budget a real bound: a larger buffer would let the
	// server queue bodies from beyond the batch that was planned.
	messages := make(chan *imap.Message, min(len(requested), 20))
	done := make(chan error, 1)
	go func() {
		done <- guardedUIDFetch(ctx, c, seqset, items, messages)
	}()
	fetched := make([]syncer.FetchedMessage, 0, len(requested))
	var readErr error
	for msg := range messages {
		if msg == nil {
			continue
		}
		body := msg.GetBody(section)
		if body == nil {
			continue
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			if readErr == nil {
				readErr = fmt.Errorf("read message body mailbox %q UID %d: %w", mailbox, msg.Uid, err)
			}
			continue
		}
		fetched = append(fetched, syncer.FetchedMessage{
			Mailbox:      mailbox,
			UID:          msg.Uid,
			InternalDate: msg.InternalDate,
			Size:         int64(msg.Size),
			Flags:        msg.Flags,
			Raw:          raw,
		})
	}
	if err := <-done; err != nil {
		return fmt.Errorf("fetch mailbox %q UID batch %s: %w", mailbox, describeBatch(requested), err)
	}
	if readErr != nil {
		return readErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ordered, err := orderFetchedUIDBatch(requested, fetched)
	if err != nil {
		return fmt.Errorf("fetch mailbox %q UID batch %s: %w", mailbox, describeBatch(requested), err)
	}
	fetched = nil
	for i := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := handle(ordered[i]); err != nil {
			return fmt.Errorf("store message mailbox %q UID %d: %w", mailbox, ordered[i].UID, err)
		}
		// Drop each body as soon as it is stored. Holding the whole batch until
		// the last message has been parsed and indexed keeps the largest mails in
		// the folder resident for the entire batch.
		ordered[i].Raw = nil
	}
	return nil
}

// guardedUIDFetch prevents go-imap's absolute command deadline from remaining
// armed while fetched messages are parsed, stored, and indexed. The dependency
// has no context-aware command API, so cancellation closes the connection to
// unblock an active fetch.
func guardedUIDFetch(ctx context.Context, c *client.Client, seqset *imap.SeqSet, items []imap.FetchItem, messages chan *imap.Message) error {
	if err := ctx.Err(); err != nil {
		close(messages)
		return err
	}
	if c == nil {
		close(messages)
		return errors.New("fetch UIDs requires an IMAP client")
	}

	previousTimeout := c.Timeout
	c.Timeout = 0
	defer func() { c.Timeout = previousTimeout }()

	commandTimeout := previousTimeout
	if commandTimeout <= 0 {
		commandTimeout = defaultIMAPCommandTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	terminated := make(chan struct{})
	stopTerminate := context.AfterFunc(commandCtx, func() {
		terminateClient(c)
		close(terminated)
	})
	err := c.UidFetch(seqset, items, messages)
	if !stopTerminate() {
		<-terminated
	}
	commandErr := commandCtx.Err()
	cancel()
	if commandErr != nil {
		return commandErr
	}
	return err
}

func orderFetchedUIDBatch(requested []uint32, fetched []syncer.FetchedMessage) ([]syncer.FetchedMessage, error) {
	wanted := make(map[uint32]bool, len(requested))
	for _, uid := range requested {
		if uid != 0 {
			wanted[uid] = true
		}
	}
	byUID := make(map[uint32]syncer.FetchedMessage, len(fetched))
	for _, message := range fetched {
		if !wanted[message.UID] {
			continue
		}
		if _, exists := byUID[message.UID]; exists {
			return nil, fmt.Errorf("IMAP server returned UID %d more than once", message.UID)
		}
		byUID[message.UID] = message
	}
	ordered := make([]syncer.FetchedMessage, 0, len(requested))
	missing := make([]uint32, 0)
	for _, uid := range requested {
		message, ok := byUID[uid]
		if !ok {
			missing = append(missing, uid)
			continue
		}
		ordered = append(ordered, message)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("IMAP server omitted requested UID batch %s", describeBatch(missing))
	}
	return ordered, nil
}

func normalizeUIDList(uids []uint32) []uint32 {
	if len(uids) == 0 {
		return nil
	}
	out := append([]uint32(nil), uids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	n := 0
	var prev uint32
	for _, uid := range out {
		if uid == 0 || (n > 0 && uid == prev) {
			continue
		}
		out[n] = uid
		n++
		prev = uid
	}
	return out[:n]
}

// FetchMessage retrieves one raw message body for on-demand thread hydration or
// attachment download when the local blob has been pruned.
func (f *Fetcher) FetchMessage(ctx context.Context, account store.MailAccount, mailbox string, uid uint32) (syncer.FetchedMessage, error) {
	select {
	case <-ctx.Done():
		return syncer.FetchedMessage{}, ctx.Err()
	default:
	}
	if uid == 0 {
		return syncer.FetchedMessage{}, fmt.Errorf("fetch message requires a nonzero UID")
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return syncer.FetchedMessage{}, err
	}
	defer terminateClientOnContext(ctx, c)()
	selected, err := c.Select(mailbox, true)
	if err != nil {
		return syncer.FetchedMessage{}, fmt.Errorf("select mailbox %q read-only: %w", mailbox, err)
	}
	if selected == nil || selected.UidValidity == 0 {
		return syncer.FetchedMessage{}, fmt.Errorf("select mailbox %q returned no UIDVALIDITY", mailbox)
	}
	section := rawBodySection()
	items := []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchFlags, section.FetchItem()}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(seqset, items, messages)
	}()
	var out syncer.FetchedMessage
	found := false
	for msg := range messages {
		select {
		case <-ctx.Done():
			return syncer.FetchedMessage{}, ctx.Err()
		default:
		}
		if msg == nil || msg.Uid != uid {
			continue
		}
		body := msg.GetBody(section)
		if body == nil {
			continue
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			return syncer.FetchedMessage{}, fmt.Errorf("read message body mailbox %q UID %d: %w", mailbox, uid, err)
		}
		out = syncer.FetchedMessage{
			Mailbox:      mailbox,
			UID:          msg.Uid,
			UIDValidity:  selected.UidValidity,
			InternalDate: msg.InternalDate,
			Size:         int64(msg.Size),
			Flags:        msg.Flags,
			Raw:          raw,
		}
		found = true
	}
	if err := <-done; err != nil {
		return syncer.FetchedMessage{}, fmt.Errorf("fetch mailbox %q UID %d: %w", mailbox, uid, err)
	}
	if !found {
		return syncer.FetchedMessage{}, fmt.Errorf("message not found mailbox %q UID %d", mailbox, uid)
	}
	return out, nil
}

// rawBodySection fetches message data without implicitly setting the IMAP
// \Seen flag. Rolltop changes remote read state only through its explicit
// read-state synchronization path.
func rawBodySection() *imap.BodySectionName {
	return &imap.BodySectionName{Peek: true}
}

// AppendMessage copies an already-sent RFC822 message into a remote mailbox and
// returns the server-assigned UID plus canonical raw body. UIDPLUS APPENDUID is
// authoritative; older servers are correlated by comparing the fetched RFC822
// bytes rather than trusting Message-ID or UIDNEXT alone.
func (f *Fetcher) AppendMessage(ctx context.Context, account store.MailAccount, mailbox string, raw []byte, messageID string, date time.Time) (syncer.FetchedMessage, error) {
	return f.AppendMessageWithFlags(ctx, account, mailbox, raw, messageID, date, []string{imap.SeenFlag})
}

// AppendMessageWithFlags copies an RFC822 payload into a remote mailbox with
// caller-provided IMAP flags. Draft saves use \Draft while sent-message copies
// keep the existing \Seen behavior.
func (f *Fetcher) AppendMessageWithFlags(ctx context.Context, account store.MailAccount, mailbox string, raw []byte, messageID string, date time.Time, flags []string) (syncer.FetchedMessage, error) {
	select {
	case <-ctx.Done():
		return syncer.FetchedMessage{}, ctx.Err()
	default:
	}
	mailbox = strings.TrimSpace(mailbox)
	if mailbox == "" || len(raw) == 0 {
		return syncer.FetchedMessage{}, fmt.Errorf("append message requires a mailbox and raw message")
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return syncer.FetchedMessage{}, err
	}
	defer terminateClientOnContext(ctx, c)()

	var beforeUIDNext, beforeUIDValidity uint32
	if status, err := c.Status(mailbox, []imap.StatusItem{imap.StatusUidNext, imap.StatusUidValidity}); err == nil && status != nil {
		beforeUIDNext = status.UidNext
		beforeUIDValidity = status.UidValidity
	}
	receipt, err := executeAppend(c, mailbox, flags, date, bytes.NewReader(raw))
	if err != nil {
		return syncer.FetchedMessage{}, fmt.Errorf("append message to mailbox %q: %w", mailbox, err)
	}
	mbox, err := c.Select(mailbox, true)
	if err != nil {
		return syncer.FetchedMessage{}, syncer.AppendApplied(fmt.Errorf("select mailbox %q after append: %w", mailbox, err))
	}
	if receipt != nil {
		if mbox == nil || mbox.UidValidity == 0 || mbox.UidValidity != receipt.UIDValidity {
			selectedUIDValidity := uint32(0)
			if mbox != nil {
				selectedUIDValidity = mbox.UidValidity
			}
			return syncer.FetchedMessage{}, syncer.AppendApplied(fmt.Errorf(
				"append mailbox %q returned APPENDUID validity %d, selected validity %d",
				mailbox, receipt.UIDValidity, selectedUIDValidity))
		}
		fetched, fetchErr := f.fetchOneUID(ctx, c, mailbox, receipt.UID)
		if fetchErr != nil {
			return syncer.FetchedMessage{}, syncer.AppendApplied(fetchErr)
		}
		fetched.UIDValidity = receipt.UIDValidity
		fetched.AppendUIDAuthoritative = true
		return fetched, nil
	}
	if mbox != nil && beforeUIDValidity > 0 && mbox.UidValidity > 0 && beforeUIDValidity != mbox.UidValidity {
		return syncer.FetchedMessage{}, syncer.AppendApplied(fmt.Errorf(
			"mailbox %q UIDVALIDITY changed from %d to %d while confirming append",
			mailbox, beforeUIDValidity, mbox.UidValidity))
	}
	if id := strings.TrimSpace(messageID); id != "" && beforeUIDNext > 0 {
		criteria := imap.NewSearchCriteria()
		criteria.Header.Set("Message-ID", id)
		uids, err := c.UidSearch(criteria)
		if err == nil && len(uids) > 0 {
			postUIDNext := uint32(0)
			if mbox != nil {
				postUIDNext = mbox.UidNext
			}
			uids = appendCandidateUIDs(uids, beforeUIDNext, postUIDNext)
			fetched, matched, fetchErr := f.fetchMatchingAppendCandidate(ctx, c, mailbox, uids, raw)
			if fetchErr != nil {
				return syncer.FetchedMessage{}, syncer.AppendApplied(fetchErr)
			}
			if matched && mbox != nil {
				fetched.UIDValidity = mbox.UidValidity
			}
			if matched {
				return fetched, nil
			}
		}
	}
	postUIDNext := uint32(0)
	if mbox != nil {
		postUIDNext = mbox.UidNext
	}
	if candidateUID, ok := appendUIDNextCandidate(beforeUIDNext, postUIDNext); ok {
		fetched, fetchErr := f.fetchOneUID(ctx, c, mailbox, candidateUID)
		if fetchErr != nil {
			return syncer.FetchedMessage{}, syncer.AppendApplied(fetchErr)
		}
		if appendRawMatches(raw, fetched.Raw) {
			fetched.UIDValidity = mbox.UidValidity
			return fetched, nil
		}
	}
	return syncer.FetchedMessage{}, syncer.AppendApplied(fmt.Errorf("sent message was appended to mailbox %q, but its UID could not be confirmed", mailbox))
}

type appendExecutor interface {
	Execute(imap.Commander, responses.Handler) (*imap.StatusResp, error)
}

const appendUIDResponseCode imap.StatusRespCode = "APPENDUID"

type appendReceipt struct {
	UIDValidity uint32
	UID         uint32
}

// executeAppend preserves the distinction the high-level client.Append API
// drops: a tagged NO/BAD is definitive, while a transport failure before the
// tagged response leaves the remote outcome unknown. A successful UIDPLUS
// response also carries the exact destination mailbox generation and UID.
func executeAppend(c appendExecutor, mailbox string, flags []string, date time.Time, message imap.Literal) (*appendReceipt, error) {
	status, err := c.Execute(&commands.Append{
		Mailbox: mailbox,
		Flags:   flags,
		Date:    date,
		Message: message,
	}, nil)
	if err != nil {
		return nil, syncer.AppendOutcomeUnknown(err)
	}
	if status == nil {
		return nil, syncer.AppendOutcomeUnknown(errors.New("IMAP connection closed before APPEND completed"))
	}
	if err := status.Err(); err != nil {
		return nil, err
	}
	return parseAppendReceipt(status), nil
}

func parseAppendReceipt(status *imap.StatusResp) *appendReceipt {
	if status == nil || !strings.EqualFold(string(status.Code), string(appendUIDResponseCode)) || len(status.Arguments) != 2 {
		return nil
	}
	uidValidityValue, ok := responseAtom(status.Arguments[0])
	if !ok {
		return nil
	}
	uidValidity, err := strconv.ParseUint(uidValidityValue, 10, 32)
	if err != nil || uidValidity == 0 {
		return nil
	}
	uidValue, ok := responseAtom(status.Arguments[1])
	if !ok {
		return nil
	}
	uid, ok := singleStaticUID(uidValue)
	if !ok {
		return nil
	}
	return &appendReceipt{UIDValidity: uint32(uidValidity), UID: uid}
}

// Only UIDs allocated between the pre-APPEND STATUS and post-APPEND SELECT can
// belong to this operation. Without both bounds, Message-ID matches may be old.
func appendCandidateUIDs(uids []uint32, beforeUIDNext, afterUIDNext uint32) []uint32 {
	if beforeUIDNext == 0 || afterUIDNext <= beforeUIDNext {
		return nil
	}
	out := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		if uid >= beforeUIDNext && uid < afterUIDNext {
			out = append(out, uid)
		}
	}
	return out
}

func appendUIDNextCandidate(beforeUIDNext, afterUIDNext uint32) (uint32, bool) {
	if beforeUIDNext == 0 || afterUIDNext <= beforeUIDNext || afterUIDNext <= 1 {
		return 0, false
	}
	return afterUIDNext - 1, true
}

func (f *Fetcher) fetchMatchingAppendCandidate(ctx context.Context, c *client.Client, mailbox string, uids []uint32, raw []byte) (syncer.FetchedMessage, bool, error) {
	if len(uids) == 0 {
		return syncer.FetchedMessage{}, false, nil
	}
	candidates := make([]syncer.FetchedMessage, 0, len(uids))
	if err := f.fetchUIDs(ctx, c, mailbox, uids, func(message syncer.FetchedMessage) error {
		candidates = append(candidates, message)
		return nil
	}); err != nil {
		return syncer.FetchedMessage{}, false, err
	}
	matched, ok := matchAppendedMessage(raw, candidates)
	return matched, ok, nil
}

func matchAppendedMessage(raw []byte, candidates []syncer.FetchedMessage) (syncer.FetchedMessage, bool) {
	var matched syncer.FetchedMessage
	found := false
	for _, candidate := range candidates {
		if !appendRawMatches(raw, candidate.Raw) {
			continue
		}
		if !found || candidate.UID > matched.UID {
			matched = candidate
			found = true
		}
	}
	return matched, found
}

func appendRawMatches(appended, fetched []byte) bool {
	return bytes.Equal(appended, fetched) || store.CanonicalMessageSHA256(appended) == store.CanonicalMessageSHA256(fetched)
}

func (f *Fetcher) fetchOneUID(ctx context.Context, c *client.Client, mailbox string, uid uint32) (syncer.FetchedMessage, error) {
	var out syncer.FetchedMessage
	found := false
	err := f.fetchUIDs(ctx, c, mailbox, []uint32{uid}, func(msg syncer.FetchedMessage) error {
		out = msg
		found = true
		return nil
	})
	if err != nil {
		return syncer.FetchedMessage{}, err
	}
	if !found {
		return syncer.FetchedMessage{}, fmt.Errorf("message not found mailbox %q UID %d after append", mailbox, uid)
	}
	return out, nil
}

// SetSeen is the one remote read-state mutation Rolltop intentionally allows:
// it toggles only the IMAP \Seen flag for a single UID.
func (f *Fetcher) SetSeen(ctx context.Context, account store.MailAccount, mailbox string, uid uint32, seen bool) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return err
	}
	defer terminateClientOnContext(ctx, c)()
	if _, err := c.Select(mailbox, false); err != nil {
		return fmt.Errorf("select mailbox %q read-write: %w", mailbox, err)
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	var op imap.FlagsOp = imap.AddFlags
	if !seen {
		op = imap.RemoveFlags
	}
	item := imap.FormatFlagsOp(op, true)
	if err := c.UidStore(seqset, item, []interface{}{imap.SeenFlag}, nil); err != nil {
		return fmt.Errorf("sync seen flag mailbox %q UID %d: %w", mailbox, uid, err)
	}
	return nil
}

// SeenUIDs returns the remote set of read messages so local read state can be
// reconciled after another client changes flags.
func (f *Fetcher) SeenUIDs(ctx context.Context, account store.MailAccount, mailbox string) ([]uint32, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return nil, err
	}
	defer terminateClientOnContext(ctx, c)()
	if _, err := c.Select(mailbox, true); err != nil {
		return nil, fmt.Errorf("select mailbox %q read-only for seen search: %w", mailbox, err)
	}
	criteria := imap.NewSearchCriteria()
	criteria.WithFlags = []string{imap.SeenFlag}
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search seen UIDs in mailbox %q: %w", mailbox, err)
	}
	return uids, nil
}

// SetFlagged toggles the IMAP \Flagged flag for one UID so local star changes can be pushed upstream.
func (f *Fetcher) SetFlagged(ctx context.Context, account store.MailAccount, mailbox string, uid uint32, flagged bool) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return err
	}
	defer terminateClientOnContext(ctx, c)()
	if _, err := c.Select(mailbox, false); err != nil {
		return fmt.Errorf("select mailbox %q read-write: %w", mailbox, err)
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)
	var op imap.FlagsOp = imap.AddFlags
	if !flagged {
		op = imap.RemoveFlags
	}
	item := imap.FormatFlagsOp(op, true)
	if err := c.UidStore(seqset, item, []interface{}{imap.FlaggedFlag}, nil); err != nil {
		return fmt.Errorf("sync flagged flag mailbox %q UID %d: %w", mailbox, uid, err)
	}
	return nil
}

// FlaggedUIDs returns the remote starred set. The syncer stores this locally so
// Rolltop reflects stars added by another IMAP client.
func (f *Fetcher) FlaggedUIDs(ctx context.Context, account store.MailAccount, mailbox string) ([]uint32, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return nil, err
	}
	defer terminateClientOnContext(ctx, c)()
	if _, err := c.Select(mailbox, true); err != nil {
		return nil, fmt.Errorf("select mailbox %q read-only for flagged search: %w", mailbox, err)
	}
	criteria := imap.NewSearchCriteria()
	criteria.WithFlags = []string{imap.FlaggedFlag}
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search flagged UIDs in mailbox %q: %w", mailbox, err)
	}
	return uids, nil
}

// MoveMessage uses IMAP MOVE for one UID and refuses to emulate move with copy/delete when the server lacks support.
func (f *Fetcher) MoveMessage(ctx context.Context, account store.MailAccount, sourceMailbox string, destMailbox string, uid uint32) error {
	return errors.New("generation-safe IMAP move requires an expected source UIDVALIDITY")
}

// WatchMailbox keeps an IMAP IDLE session open for one folder and invokes onChange
// when the server reports message/mailbox updates. The runner decides what to sync.
func (f *Fetcher) WatchMailbox(ctx context.Context, account store.MailAccount, mailbox string, onChange func()) error {
	if strings.TrimSpace(mailbox) == "" {
		return fmt.Errorf("watch mailbox requires a mailbox name")
	}
	c, err := f.loginWithinContext(ctx, account)
	if err != nil {
		return err
	}
	defer terminateClientOnContext(ctx, c)()
	if _, err := c.Select(mailbox, true); err != nil {
		return fmt.Errorf("select mailbox %q read-only for IDLE: %w", mailbox, err)
	}
	// IDLE is intentionally long-lived and has its own context/stop handling.
	// Restore the normal timeout before any final non-IDLE command handling.
	c.Timeout = 0
	defer func() { c.Timeout = f.commandTimeout() }()
	log.Printf("imap idle account_id=%d mailbox=%s: selected read-only", account.ID, mailbox)
	updates := make(chan client.Update, 10)
	c.Updates = updates
	for {
		if err := f.idleMailboxOnce(ctx, c, updates, mailbox, onChange); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// idleMailboxOnce runs one bounded IDLE cycle. It exits on update, timeout, or
// context cancellation so callers can restart cleanly without a stuck connection.
func (f *Fetcher) idleMailboxOnce(ctx context.Context, c *client.Client, updates <-chan client.Update, mailbox string, onChange func()) error {
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.Idle(stop, &client.IdleOptions{LogoutTimeout: -1})
	}()
	stopIdle := func() {
		_ = stopIdleSession(stop, done, c.Terminate, idleStopGrace)
	}
	timer := time.NewTimer(idleCycleDuration)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			stopIdle()
			return ctx.Err()
		case err := <-done:
			if err != nil {
				return fmt.Errorf("IDLE mailbox %q: %w", mailbox, err)
			}
			return nil
		case update, ok := <-updates:
			if !ok {
				if err := stopIdleSession(stop, done, c.Terminate, idleStopGrace); err != nil {
					return fmt.Errorf("IDLE mailbox %q: %w", mailbox, err)
				}
				return fmt.Errorf("IDLE mailbox %q updates channel closed", mailbox)
			}
			switch update.(type) {
			case *client.MailboxUpdate, *client.MessageUpdate, *client.ExpungeUpdate:
				if onChange != nil {
					onChange()
				}
			}
			if err := stopIdleSession(stop, done, c.Terminate, idleStopGrace); err != nil {
				return fmt.Errorf("IDLE mailbox %q: %w", mailbox, err)
			}
			return nil
		case <-timer.C:
			if err := stopIdleSession(stop, done, c.Terminate, idleStopGrace); err != nil {
				return fmt.Errorf("IDLE mailbox %q: %w", mailbox, err)
			}
			return nil
		}
	}
}

func stopIdleSession(stop chan struct{}, done <-chan error, terminate func() error, grace time.Duration) error {
	close(stop)
	if grace <= 0 {
		grace = idleStopGrace
	}
	timer := time.NewTimer(grace)
	select {
	case err := <-done:
		timer.Stop()
		return err
	case <-timer.C:
	}
	if terminate != nil {
		if err := terminate(); err != nil {
			return fmt.Errorf("%w: terminate connection: %v", errIdleStopTimeout, err)
		}
	}
	timer = time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %v", errIdleStopTimeout, err)
		}
		return errIdleStopTimeout
	case <-timer.C:
		return errIdleStopTimeout
	}
}

// login authenticates an account with whichever credential it is configured
// for. Both paths obtain the secret at the last possible moment: a password is
// decrypted here, and an access token is minted here, so neither sits in memory
// while a connection is merely being considered.
func (f *Fetcher) login(account store.MailAccount) (*client.Client, error) {
	if account.UsesGoogleOAuth() {
		return f.loginWithGoogleToken(account)
	}
	password, err := mmcrypto.DecryptString(f.MasterKey, account.EncryptedPassword)
	if err != nil {
		return nil, fmt.Errorf("decrypt IMAP password: %w", err)
	}
	return f.connectAndAuthenticate(account, func(c *client.Client) error {
		return c.Login(account.Username, password)
	})
}

// loginWithGoogleToken authenticates with a Google access token, letting the
// shared helper own the refresh-and-retry policy the SMTP path uses too.
func (f *Fetcher) loginWithGoogleToken(account store.MailAccount) (*client.Client, error) {
	// login has no context of its own and runs inside a goroutine that
	// loginByContext abandons on cancellation, so the token work is bounded by
	// the same command timeout the connection uses.
	ctx, cancel := context.WithTimeout(context.Background(), f.commandTimeout())
	defer cancel()
	var authenticated *client.Client
	err := xoauth2.WithFreshToken(ctx, f.Tokens, account.UserID, account.GoogleConnectionID,
		func(token string) error {
			c, err := f.connectAndAuthenticate(account, authenticateXOAUTH2(account, token))
			if err != nil {
				return err
			}
			authenticated = c
			return nil
		})
	if err != nil {
		return nil, err
	}
	return authenticated, nil
}

func authenticateXOAUTH2(account store.MailAccount, token string) func(*client.Client) error {
	return func(c *client.Client) error {
		supported, err := c.SupportAuth(xoauth2.Mechanism)
		if err != nil {
			return err
		}
		if !supported {
			return fmt.Errorf("%w for %s", xoauth2.ErrUnsupported, account.Host)
		}
		return c.Authenticate(xoauth2.NewSASLClient(account.Username, token))
	}
}

func (f *Fetcher) connectAndAuthenticate(account store.MailAccount, authenticate func(*client.Client) error) (*client.Client, error) {
	var err error
	addr := net.JoinHostPort(account.Host, fmt.Sprintf("%d", account.Port))
	timeout := f.commandTimeout()

	var c *client.Client
	if account.UseTLS {
		dialer := &net.Dialer{Timeout: timeout}
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: account.Host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return nil, fmt.Errorf("connect TLS to IMAP server %s: %w", addr, err)
		}
		if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("set IMAP greeting deadline for %s: %w", addr, err)
		}
		c, err = client.New(conn)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("initialize IMAP client for %s: %w", addr, err)
		}
		if err := conn.SetDeadline(time.Time{}); err != nil {
			terminateClient(c)
			return nil, fmt.Errorf("clear IMAP greeting deadline for %s: %w", addr, err)
		}
	} else {
		c, err = client.DialWithDialer(&net.Dialer{Timeout: timeout}, addr)
		if err != nil {
			return nil, fmt.Errorf("connect plain IMAP to server %s: %w", addr, err)
		}
	}
	c.Timeout = timeout
	if err := authenticate(c); err != nil {
		terminateClient(c)
		wrapped := fmt.Errorf("login to IMAP server %s: %w", addr, err)
		// A server that does not offer XOAUTH2 has not rejected the credential,
		// so this must not look like a stale token: the retry would spend a real
		// refresh at Google and open a second connection that fails identically,
		// once per mailbox.
		if errors.Is(err, xoauth2.ErrUnsupported) {
			return nil, wrapped
		}
		return nil, xoauth2.AuthError{Err: wrapped}
	}
	return c, nil
}

// loginWithinContext bounds dialing and authentication by the shorter of the
// configured command timeout and the caller's deadline. Once connected, callers
// should also use terminateClientOnContext so cancellation interrupts a command
// already in progress.
func (f *Fetcher) loginWithinContext(ctx context.Context, account store.MailAccount) (*client.Client, error) {
	bounded, err := f.boundedByContext(ctx)
	if err != nil {
		return nil, err
	}
	return bounded.loginByContext(ctx, account)
}

// terminateClientOnContext makes one-shot IMAP sessions obey cancellation even
// though go-imap v1 has no context-aware command API. Cleanup closes the socket
// directly instead of issuing LOGOUT, which could otherwise add another blocked
// network command after the useful work has already failed or completed.
func terminateClientOnContext(ctx context.Context, c *client.Client) func() {
	if c == nil {
		return func() {}
	}
	stopWatching := watchClientContext(ctx, c)
	return func() {
		stopWatching()
		terminateClient(c)
	}
}

// terminateClient marks a locally initiated close before dropping the socket.
// go-imap otherwise logs the resulting net.ErrClosed as if the peer failed.
func terminateClient(c *client.Client) error {
	if c == nil {
		return nil
	}
	c.SetState(imap.LogoutState, nil)
	return c.Terminate()
}

func watchClientContext(ctx context.Context, c *client.Client) func() {
	if c == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	terminated := make(chan struct{})
	stopTerminate := context.AfterFunc(ctx, func() {
		terminateClient(c)
		close(terminated)
	})
	return func() {
		if !stopTerminate() {
			<-terminated
		}
	}
}

func (f *Fetcher) commandTimeout() time.Duration {
	if f != nil && f.Timeout > 0 {
		return f.Timeout
	}
	return defaultIMAPCommandTimeout
}

func hasAttr(attrs []string, target string) bool {
	for _, attr := range attrs {
		if strings.EqualFold(attr, target) {
			return true
		}
	}
	return false
}

func describeBatch(uids []uint32) string {
	if len(uids) == 0 {
		return "empty"
	}
	if len(uids) == 1 {
		return fmt.Sprintf("%d", uids[0])
	}
	return fmt.Sprintf("%d..%d (%d messages)", uids[0], uids[len(uids)-1], len(uids))
}
