// File overview: Contact sync against a fake People API. No test here talks to
// Google: the fake is the contract, and every behaviour that matters -- what a
// delta deletes, what a full read prunes, who wins a conflict -- is asserted
// against it rather than against a live account nobody can run in CI.

package googlepeople

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rolltop/backend/googletoken"
	"rolltop/backend/store"
)

// fakePeople serves the two endpoints the sync uses. Pages and sync tokens are
// scripted per call so a test can describe a sequence of Google states.
type fakePeople struct {
	mu sync.Mutex
	// responses is the scripted reply for each successive connections.list
	// call; the last entry repeats.
	responses []ConnectionsPage
	// listStatus lets a test force an error status on the nth list call.
	listStatus []int
	calls      int
	// syncTokens records the syncToken query parameter of each list call, and
	// requestedToken whether that call asked for a new cursor.
	syncTokens     []string
	requestedToken []bool
	// people answers GetPerson and receives writes.
	people map[string]Person
	// updates counts accepted updateContact calls.
	updates int
	// updateConflict makes the next update fail with a stale-etag precondition.
	updateConflict bool
	deleted        []string
	// holdFirstList, when set, blocks the first list call until the channel is
	// closed. It is what lets a test hold one sync mid-flight and observe what
	// a second one does in the meantime.
	holdFirstList chan struct{}
	// listStarted counts list requests as they arrive, before any hold. The
	// calls counter only moves once a request completes, so a held request
	// would otherwise be invisible to the test that is holding it.
	listStarted int
}

func (f *fakePeople) startedListCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listStarted
}

func (f *fakePeople) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/people/me/connections", func(w http.ResponseWriter, r *http.Request) {
		// The barrier waits outside the lock so a held first call cannot
		// deadlock the very second call the test wants to observe.
		f.mu.Lock()
		hold := f.holdFirstList
		first := f.listStarted == 0
		f.listStarted++
		f.mu.Unlock()
		if first && hold != nil {
			<-hold
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.syncTokens = append(f.syncTokens, r.URL.Query().Get("syncToken"))
		// Google returns a nextSyncToken only when the request asked for one.
		// Answering with a cursor the real API would have withheld is exactly
		// how a missing requestSyncToken stays invisible in tests.
		f.requestedToken = append(f.requestedToken, r.URL.Query().Get("requestSyncToken") == "true")
		index := f.calls
		f.calls++
		if index < len(f.listStatus) && f.listStatus[index] != 0 {
			writeGoogleError(w, f.listStatus[index], "FAILED_PRECONDITION")
			return
		}
		page := ConnectionsPage{}
		if index < len(f.responses) {
			page = f.responses[index]
		} else if len(f.responses) > 0 {
			page = f.responses[len(f.responses)-1]
		}
		if r.URL.Query().Get("requestSyncToken") != "true" {
			page.NextSyncToken = ""
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("/v1/people/", func(w http.ResponseWriter, r *http.Request) {
		resource := strings.TrimPrefix(r.URL.Path, "/v1/")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(resource, ":updateContact"):
			name := strings.TrimSuffix(resource, ":updateContact")
			if f.updateConflict {
				f.updateConflict = false
				writeGoogleError(w, http.StatusBadRequest, "FAILED_PRECONDITION")
				return
			}
			var incoming Person
			_ = json.NewDecoder(r.Body).Decode(&incoming)
			incoming.ResourceName = name
			incoming.ETag = "etag-updated"
			f.people[name] = incoming
			f.updates++
			_ = json.NewEncoder(w).Encode(incoming)
		case strings.HasSuffix(resource, ":deleteContact"):
			f.deleted = append(f.deleted, strings.TrimSuffix(resource, ":deleteContact"))
			_, _ = w.Write([]byte(`{}`))
		default:
			person, ok := f.people[resource]
			if !ok {
				writeGoogleError(w, http.StatusNotFound, "NOT_FOUND")
				return
			}
			_ = json.NewEncoder(w).Encode(person)
		}
	})
	mux.HandleFunc("/v1/people:createContact", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var incoming Person
		_ = json.NewDecoder(r.Body).Decode(&incoming)
		incoming.ResourceName = "people/created1"
		incoming.ETag = "etag-created"
		f.people[incoming.ResourceName] = incoming
		_ = json.NewEncoder(w).Encode(incoming)
	})
	return mux
}

func writeGoogleError(w http.ResponseWriter, status int, reason string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"status": reason, "message": reason},
	})
}

// stubConnections stands in for the auth manager. It hands back a connection
// with whatever scope the test wants without going near stored tokens.
type stubConnections struct {
	connection store.GoogleConnection
}

func (s stubConnections) List(context.Context, int64) ([]store.GoogleConnection, error) {
	return []store.GoogleConnection{s.connection}, nil
}

func (s stubConnections) Get(_ context.Context, _, connectionID int64) (store.GoogleConnection, error) {
	if connectionID != s.connection.ID {
		return store.GoogleConnection{}, store.ErrNotFound
	}
	return s.connection, nil
}

func newSyncFixture(t *testing.T, fake *fakePeople) (*Syncer, *store.Store, store.User) {
	t.Helper()
	if fake.people == nil {
		fake.people = map[string]Person{}
	}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	db, err := store.Open(filepath.Join(t.TempDir(), "rolltop.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	user, err := db.CreateUser(context.Background(), "contacts-owner@example.test", "Contacts Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient()
	client.BaseURL = server.URL
	client.RetryDelay = func(int) time.Duration { return time.Millisecond }
	syncer := &Syncer{
		Store:  db,
		Client: client,
		Tokens: &googletoken.StubTokenSource{Tokens: []string{"access-token"}},
		Connections: stubConnections{connection: store.GoogleConnection{
			ID: 7, UserID: user.ID, GoogleEmail: "owner@gmail.example.test", Status: store.GoogleConnectionStatusOK,
		}},
		ScopeGranted: func(store.GoogleConnection) bool { return true },
	}
	return syncer, db, user
}

func personWithEmail(resource, etag, name, email string) Person {
	return Person{
		ResourceName: resource,
		ETag:         etag,
		Names:        []Name{{DisplayName: name, GivenName: name}},
		Emails:       []EmailAddress{{Value: email, Metadata: &FieldMetadata{Primary: true}}},
	}
}

func contactByEmail(t *testing.T, db *store.Store, userID int64, email string) (store.Contact, bool) {
	t.Helper()
	contact, err := db.GetContactByEmailForUser(context.Background(), userID, email)
	if store.IsNotFound(err) {
		return store.Contact{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return contact, true
}

// The first run has no cursor, so it reads everything and stores the token the
// next run needs. Without that token every run would be a full read.
func TestSyncStoresTheCursorFromTheFirstFullRead(t *testing.T) {
	fake := &fakePeople{responses: []ConnectionsPage{{
		People:        []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")},
		NextSyncToken: "token-1",
	}}}
	syncer, db, user := newSyncFixture(t, fake)

	result, err := syncer.SyncConnection(context.Background(), user.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !result.FullSync || result.Created != 1 {
		t.Fatalf("first sync = %+v, want one created contact from a full read", result)
	}
	contact, ok := contactByEmail(t, db, user.ID, "ada@example.test")
	if !ok {
		t.Fatal("contact was not created")
	}
	if !contact.IsGoogleContact() || contact.ExternalID != "people/c1" || contact.ETag != "etag-1" {
		t.Fatalf("contact = %+v, want a Google mirror carrying the resource name and etag", contact)
	}
	state, err := db.GetGooglePeopleSync(context.Background(), user.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if state.SyncToken != "token-1" || state.Status != store.GooglePeopleSyncStatusOK {
		t.Fatalf("sync state = %+v, want the stored cursor and an ok status", state)
	}
}

// A delta must apply removals. Google reports them as tombstones rather than by
// omitting the person, and treating a tombstone as an ordinary contact would
// leave a nameless row behind instead of deleting anything.
func TestSyncDeletesContactsGoogleReportsAsDeleted(t *testing.T) {
	fake := &fakePeople{responses: []ConnectionsPage{
		{People: []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")}, NextSyncToken: "token-1"},
		{People: []Person{{ResourceName: "people/c1", Metadata: &PersonMetadata{Deleted: true}}}, NextSyncToken: "token-2"},
	}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	result, err := syncer.SyncConnection(ctx, user.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if result.FullSync {
		t.Fatal("second sync read everything again instead of using the stored cursor")
	}
	if result.Deleted != 1 {
		t.Fatalf("second sync = %+v, want one deletion", result)
	}
	if _, ok := contactByEmail(t, db, user.ID, "ada@example.test"); ok {
		t.Fatal("the deleted contact is still in the address book")
	}
	if len(fake.syncTokens) < 2 || fake.syncTokens[1] != "token-1" {
		t.Fatalf("sync tokens sent = %v, want the second call to carry token-1", fake.syncTokens)
	}
}

// Google discards a cursor after about a week. Reporting that as a failure
// would leave the connection stuck on a token it can never use again, so the
// run has to fall back to a full read on its own.
func TestSyncFallsBackToAFullReadWhenTheCursorExpires(t *testing.T) {
	fake := &fakePeople{
		responses: []ConnectionsPage{
			{People: []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")}, NextSyncToken: "token-1"},
			{},
			{People: []Person{personWithEmail("people/c2", "etag-2", "Grace", "grace@example.test")}, NextSyncToken: "token-3"},
		},
		listStatus: []int{0, http.StatusGone, 0},
	}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	result, err := syncer.SyncConnection(ctx, user.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !result.FullSync {
		t.Fatalf("recovery sync = %+v, want a full read", result)
	}
	// The full read is authoritative: a contact it never mentions is gone.
	if _, ok := contactByEmail(t, db, user.ID, "ada@example.test"); ok {
		t.Fatal("a contact absent from the full read survived it")
	}
	if _, ok := contactByEmail(t, db, user.ID, "grace@example.test"); !ok {
		t.Fatal("the contact from the full read was not stored")
	}
}

// Every call asks Google for a cursor, including a delta. A request that
// forgets to would be answered without a nextSyncToken, and the connection
// would fall back to reading the whole address book on every second poll.
func TestEveryReadAsksForACursor(t *testing.T) {
	person := personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")
	fake := &fakePeople{responses: []ConnectionsPage{
		{People: []Person{person}, NextSyncToken: "token-1"},
		{NextSyncToken: "token-2"},
	}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	requested := append([]bool(nil), fake.requestedToken...)
	fake.mu.Unlock()
	if len(requested) != 2 || !requested[0] || !requested[1] {
		t.Fatalf("requestSyncToken per call = %v, want it on every read", requested)
	}
	state, err := db.GetGooglePeopleSync(ctx, user.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if state.SyncToken != "token-2" {
		t.Fatalf("stored cursor = %q, want the delta's own token", state.SyncToken)
	}
}

// A run that comes back without a cursor must not wipe the stored one. Doing so
// would silently downgrade the next poll to a full read of everything.
func TestASyncWithoutANewCursorKeepsTheStoredOne(t *testing.T) {
	fake := &fakePeople{responses: []ConnectionsPage{
		{People: []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")}, NextSyncToken: "token-1"},
		{},
	}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	state, err := db.GetGooglePeopleSync(ctx, user.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if state.SyncToken != "token-1" {
		t.Fatalf("stored cursor = %q, want the previous one kept", state.SyncToken)
	}
}

// Removing a phone number at Google has to remove it here. Folding the stored
// copy back in would resurrect it on every sync, and the next local edit would
// push it back into the user's Google account.
func TestSyncAppliesDeletionsWithinAnAlreadyMirroredContact(t *testing.T) {
	full := personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")
	full.Phones = []PhoneNumber{{Value: "+1 555 0100", Type: "mobile"}}
	full.Biographies = []Biography{{Value: "Analyst"}}
	trimmed := personWithEmail("people/c1", "etag-2", "Ada", "ada@example.test")

	fake := &fakePeople{responses: []ConnectionsPage{
		{People: []Person{full}, NextSyncToken: "token-1"},
		{People: []Person{trimmed}, NextSyncToken: "token-2"},
	}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	contact, ok := contactByEmail(t, db, user.ID, "ada@example.test")
	if !ok {
		t.Fatal("the contact disappeared")
	}
	if len(contact.Phones) != 0 {
		t.Fatalf("phones = %+v, want the number deleted at Google gone here too", contact.Phones)
	}
	if contact.Notes != "" {
		t.Fatalf("notes = %q, want the biography cleared at Google cleared here too", contact.Notes)
	}
}

// An address book that already knows someone must not gain a second copy of
// them, and the identity flags that outgoing mail hangs off have to survive the
// promotion.
func TestSyncPromotesAMatchingLocalContactInsteadOfDuplicating(t *testing.T) {
	fake := &fakePeople{responses: []ConnectionsPage{{
		People:        []Person{personWithEmail("people/c1", "etag-1", "Ada Lovelace", "ada@example.test")},
		NextSyncToken: "token-1",
	}}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	local, err := db.CreateContact(ctx, user.ID, store.Contact{
		DisplayName: "Ada",
		IsMe:        true,
		IsPrimary:   true,
		Notes:       "Met at the conference",
		Emails:      []store.ContactEmail{{Label: "Email", Email: "ada@example.test", IsPrimary: true}},
		Phones:      []store.ContactPhone{{Label: "Mobile", Number: "+1 555 0100"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	contacts, err := db.ListContactsForUser(ctx, user.ID, store.ContactListFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(contacts) != 1 {
		t.Fatalf("address book holds %d contacts, want the single promoted one", len(contacts))
	}
	promoted := contacts[0]
	if promoted.ID != local.ID {
		t.Fatalf("promoted contact id = %d, want the existing %d", promoted.ID, local.ID)
	}
	if !promoted.IsGoogleContact() || promoted.ExternalID != "people/c1" {
		t.Fatalf("promoted contact = %+v, want it linked to the Google person", promoted)
	}
	if !promoted.IsMe || !promoted.IsPrimary {
		t.Fatal("promotion dropped the Me flags an outgoing identity depends on")
	}
	if promoted.DisplayName != "Ada Lovelace" {
		t.Fatalf("display name = %q, want Google's version to win", promoted.DisplayName)
	}
	// Google has no phone number for this person; dropping the local one would
	// lose data the user entered here and Google never had a chance to keep.
	if len(promoted.Phones) != 1 || promoted.Phones[0].Number != "+1 555 0100" {
		t.Fatalf("phones = %+v, want the local number preserved", promoted.Phones)
	}
	if promoted.Notes != "Met at the conference" {
		t.Fatalf("notes = %q, want the local note preserved where Google has none", promoted.Notes)
	}
}

// A full read re-reports every person. Rewriting all of them each time would
// churn the database and bump updated_at on contacts nothing changed about.
func TestSyncSkipsContactsWhoseETagIsUnchanged(t *testing.T) {
	person := personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")
	fake := &fakePeople{responses: []ConnectionsPage{
		{People: []Person{person}, NextSyncToken: "token-1"},
		{People: []Person{person}, NextSyncToken: "token-2"},
	}}
	syncer, _, user := newSyncFixture(t, fake)
	ctx := context.Background()

	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	result, err := syncer.SyncConnection(ctx, user.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created != 0 || result.Updated != 0 {
		t.Fatalf("second sync = %+v, want no writes for an unchanged contact", result)
	}
}

// Contact rows are per-tenant, and a sync that reached another user's address
// book would be the worst kind of leak: their contacts, silently merged.
func TestSyncOnlyTouchesTheOwningTenant(t *testing.T) {
	fake := &fakePeople{responses: []ConnectionsPage{{
		People:        []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")},
		NextSyncToken: "token-1",
	}}}
	syncer, db, user := newSyncFixture(t, fake)
	ctx := context.Background()

	other, err := db.CreateUser(ctx, "other-owner@example.test", "Other Owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateContact(ctx, other.ID, store.Contact{
		DisplayName: "Ada",
		Emails:      []store.ContactEmail{{Label: "Email", Email: "ada@example.test", IsPrimary: true}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := syncer.SyncConnection(ctx, user.ID, 7); err != nil {
		t.Fatal(err)
	}
	neighbour, ok := contactByEmail(t, db, other.ID, "ada@example.test")
	if !ok {
		t.Fatal("the other tenant's contact disappeared")
	}
	if neighbour.IsGoogleContact() {
		t.Fatalf("the other tenant's contact was linked to this user's Google account: %+v", neighbour)
	}
	mine, ok := contactByEmail(t, db, user.ID, "ada@example.test")
	if !ok || !mine.IsGoogleContact() {
		t.Fatalf("owning tenant's contact = %+v, want it linked", mine)
	}
}

// A connection whose grant predates contact sync still works for mail. Failing
// its sync as an ordinary error would hide the one thing that fixes it.
func TestSyncRefusesAConnectionWithoutTheContactsScope(t *testing.T) {
	fake := &fakePeople{}
	syncer, _, user := newSyncFixture(t, fake)
	syncer.ScopeGranted = func(store.GoogleConnection) bool { return false }

	if _, err := syncer.SyncConnection(context.Background(), user.ID, 7); err == nil {
		t.Fatal("a connection without the contacts scope was allowed to sync")
	} else if !strings.Contains(err.Error(), "contacts") {
		t.Fatalf("error = %v, want it to name the missing contacts access", err)
	}
	fake.mu.Lock()
	calls := fake.calls
	fake.mu.Unlock()
	if calls != 0 {
		t.Fatalf("called Google %d times without the scope, want none", calls)
	}
}

// The poll and the Sync now button reach the same connection, and both start
// with a lookup that says a person is missing. Run at once they both act on
// that answer, and the second insert loses to the unique index over the
// resource name -- which used to fail the whole run and leave the address book
// as empty as it was before.
//
// The overlap is forced, not hoped for: the first sync is held mid-list while
// the second starts, so the test fails if serialization is ever removed rather
// than passing whenever the first sync happens to finish early.
func TestConcurrentSyncsOfOneConnectionDoNotCollide(t *testing.T) {
	hold := make(chan struct{})
	fake := &fakePeople{
		holdFirstList: hold,
		responses: []ConnectionsPage{{
			People: []Person{
				personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test"),
				personWithEmail("people/c2", "etag-2", "Grace", "grace@example.test"),
				personWithEmail("people/c3", "etag-3", "Alan", "alan@example.test"),
			},
			NextSyncToken: "token-1",
		}},
	}
	syncer, db, user := newSyncFixture(t, fake)
	// The fixture's cleanup closes the test server, which waits for the held
	// request; release the barrier first even when an assertion fails.
	t.Cleanup(func() {
		select {
		case <-hold:
		default:
			close(hold)
		}
	})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, errs[index] = syncer.SyncConnection(context.Background(), user.ID, 7)
		}(i)
	}
	// One sync is held inside Google's list call; the other must be waiting at
	// the gate rather than listing too. Poll briefly: the goroutines need a
	// moment to start at all.
	deadline := time.Now().Add(2 * time.Second)
	for fake.startedListCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if started := fake.startedListCalls(); started != 1 {
		t.Fatalf("list calls while one sync is held = %d, want 1: the second sync must wait at the gate", started)
	}
	close(hold)
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent sync %d: %v", index, err)
		}
	}

	contacts, err := db.ListContactsForUser(context.Background(), user.ID, store.ContactListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(contacts) != 3 {
		t.Fatalf("stored %d contacts, want the three Google returned exactly once", len(contacts))
	}
	for _, email := range []string{"ada@example.test", "grace@example.test", "alan@example.test"} {
		contact, ok := contactByEmail(t, db, user.ID, email)
		if !ok || !contact.IsGoogleContact() {
			t.Fatalf("%s = %+v, want a Google mirror", email, contact)
		}
	}
}

// A caller whose context ends while another sync holds the gate walks away with
// that context's error. Standing in line would make a Sync now press wait out
// the whole background sync only to fail afterwards anyway.
func TestAWaitingSyncReturnsWhenItsContextEnds(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	fake := &fakePeople{
		holdFirstList: hold,
		responses: []ConnectionsPage{{
			People:        []Person{personWithEmail("people/c1", "etag-1", "Ada", "ada@example.test")},
			NextSyncToken: "token-1",
		}},
	}
	syncer, _, user := newSyncFixture(t, fake)

	go func() {
		_, _ = syncer.SyncConnection(context.Background(), user.ID, 7)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for fake.startedListCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := syncer.SyncConnection(ctx, user.ID, 7)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting sync returned %v, want the caller's cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled caller kept waiting for the running sync to finish")
	}
}
