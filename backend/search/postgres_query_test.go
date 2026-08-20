package search

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

func TestPGTSQueryRendering(t *testing.T) {
	cases := []struct {
		text       string
		quoted     bool
		prefixLast bool
		want       string
	}{
		{"Rechnung", false, true, "'rechnung':*"},
		{"Rechnung Januar", false, true, "'rechnung' & 'januar':*"},
		{"Rechnung Januar", false, false, "'rechnung' & 'januar'"},
		{"Rechnung Januar", true, false, "'rechnung' <-> 'januar'"},
		{"  ", false, true, ""},
		{"O'Brien & Söhne", false, false, "'o' & 'brien' & 'söhne'"},
		{"drop);--table", false, false, "'drop' & 'table'"},
	}
	for _, c := range cases {
		if got := pgTSQuery(c.text, c.quoted, c.prefixLast); got != c.want {
			t.Errorf("pgTSQuery(%q, quoted=%v, prefix=%v) = %q, want %q", c.text, c.quoted, c.prefixLast, got, c.want)
		}
	}
}

func TestEscapeLikePattern(t *testing.T) {
	if got := escapeLikePattern(`50%_of\things`); got != `50\%\_of\\things` {
		t.Fatalf("escaped = %q", got)
	}
}

func TestPostgresBackendReadPath(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()

	invoice := seedPostgresSearchMessage(t, db, user, mailbox, 10, "Rechnung Januar", "anbei die rechnung für den januar")
	minutes := seedPostgresSearchMessage(t, db, user, mailbox, 11, "Protokoll", "im anhang die rechnung als nachtrag")
	unrelated := seedPostgresSearchMessage(t, db, user, mailbox, 12, "Newsletter", "angebote der woche")

	// The row flag and the attachments row are what the sync maintains and
	// what has:attachment and filename: read.
	if _, err := db.DB().ExecContext(ctx, `UPDATE messages SET has_attachments = 1 WHERE id = $1`, invoice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAttachment(ctx, store.Attachment{
		UserID: user.ID, MessageID: invoice.ID, BlobID: invoice.BlobID,
		Filename: "rechnung-januar.pdf", ContentType: "application/pdf", Size: 1, BlobPath: invoice.BlobPath,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{
		{Message: invoice, Attachments: []AttachmentDoc{{Filename: "rechnung-januar.pdf", ContentType: "application/pdf", Text: "betrag 100 euro"}}},
		{Message: minutes},
		{Message: unrelated},
	}); err != nil {
		t.Fatalf("index: %v", err)
	}

	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "rechnung", 10, 0, SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	// The subject match must outrank the body-only match.
	if hits[0].ID != invoice.ID || hits[1].ID != minutes.ID {
		t.Fatalf("order = %d, %d; want subject match %d first", hits[0].ID, hits[1].ID, invoice.ID)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("scores = %v, %v; want subject match scored higher", hits[0].Score, hits[1].Score)
	}
	foundSubject := false
	for _, field := range hits[0].Fields {
		if field == "subject" {
			foundSubject = true
		}
	}
	if !foundSubject {
		t.Fatalf("top hit fields = %v, want subject", hits[0].Fields)
	}

	// Prefix on the final term serves as-you-type search.
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rechn", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 2 {
		t.Fatalf("prefix hits = %d, err = %v, want 2", len(hits), err)
	}

	// A quoted phrase requires adjacency.
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, `"rechnung januar"`, 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].ID != invoice.ID {
		t.Fatalf("phrase hits = %v, err = %v, want just %d", hits, err, invoice.ID)
	}

	// Negation excludes.
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rechnung -protokoll", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].ID != invoice.ID {
		t.Fatalf("negated hits = %v, err = %v, want just %d", hits, err, invoice.ID)
	}

	// Operators read the live messages row.
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rechnung has:attachment", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].ID != invoice.ID {
		t.Fatalf("has:attachment hits = %v, err = %v, want just %d", hits, err, invoice.ID)
	}
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rechnung filename:januar", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].ID != invoice.ID {
		t.Fatalf("filename hits = %v, err = %v, want just %d", hits, err, invoice.ID)
	}
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rechnung subject:protokoll", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].ID != minutes.ID {
		t.Fatalf("subject filter hits = %v, err = %v, want just %d", hits, err, minutes.ID)
	}
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rechnung from:alice", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 2 {
		t.Fatalf("from filter hits = %d, err = %v, want 2", len(hits), err)
	}

	// Another tenant sees nothing.
	other, err := db.CreateUser(ctx, "pg-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	hits, err = svc.SearchHitsWithOptions(ctx, other.ID, "rechnung", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 0 {
		t.Fatalf("cross-tenant hits = %v, err = %v, want none", hits, err)
	}

	// Match answers for one message with the same grammar.
	hit, ok, err := svc.MatchMessageWithOptions(ctx, user.ID, invoice.ID, "rechnung", SearchOptions{})
	if err != nil || !ok || hit.ID != invoice.ID {
		t.Fatalf("match = %+v, ok = %v, err = %v", hit, ok, err)
	}
	_, ok, err = svc.MatchMessageWithOptions(ctx, user.ID, unrelated.ID, "rechnung", SearchOptions{})
	if err != nil || ok {
		t.Fatalf("non-matching message reported a match, ok = %v, err = %v", ok, err)
	}

	// Explain names the matched fields.
	explanation, ok, err := svc.ExplainMessageWithOptions(ctx, user.ID, invoice.ID, "rechnung", SearchOptions{})
	if err != nil || !ok {
		t.Fatalf("explain ok = %v, err = %v", ok, err)
	}
	if len(explanation.FieldMatches) == 0 || explanation.Score <= 0 {
		t.Fatalf("explanation = %+v, want field matches and a positive score", explanation)
	}
}

func TestPostgresBackendSenderBoostReordersHits(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()

	// Two body-only matches, identical text weight; the boost decides.
	plain := seedPostgresSearchMessage(t, db, user, mailbox, 20, "Erste", "stichwort inhalt")
	boosted := seedPostgresSearchMessage(t, db, user, mailbox, 21, "Zweite", "stichwort inhalt")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: plain}, {Message: boosted}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE messages SET from_addr = 'chef@firma.test' WHERE id = $1`, boosted.ID); err != nil {
		t.Fatal(err)
	}

	opts := SearchOptions{SenderBoosts: []SenderBoost{{Sender: "chef@firma.test", Boost: 5}}}
	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "stichwort", 10, 0, opts)
	if err != nil || len(hits) != 2 {
		t.Fatalf("hits = %d, err = %v, want 2", len(hits), err)
	}
	if hits[0].ID != boosted.ID {
		t.Fatalf("order = %d first, want boosted %d", hits[0].ID, boosted.ID)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("scores = %v, %v; want boost visible", hits[0].Score, hits[1].Score)
	}
}

func TestPostgresBackendSimilarity(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()

	current := seedPostgresSearchMessage(t, db, user, mailbox, 30, "Serverwartung Fenster", "wartung am samstag")
	related := seedPostgresSearchMessage(t, db, user, mailbox, 31, "Serverwartung Rückfrage", "wann ist die wartung")
	offTopic := seedPostgresSearchMessage(t, db, user, mailbox, 32, "Kantinenplan", "essen der woche")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: current}, {Message: related}, {Message: offTopic}}); err != nil {
		t.Fatalf("index: %v", err)
	}

	results, err := svc.SimilarMessages(ctx, db, user.ID, plugins.SimilarMessagesRequest{
		CurrentMessageID:    current.ID,
		CandidateMessageIDs: []int64{related.ID, offTopic.ID},
		Terms: []plugins.SimilarityTerm{
			{Field: plugins.SimilarityFieldSubject, Text: "Serverwartung", Weight: 3},
			{Field: plugins.SimilarityFieldBody, Text: "wartung", Weight: 1},
		},
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("similar: %v", err)
	}
	if len(results) != 1 || results[0].MessageID != related.ID {
		t.Fatalf("results = %+v, want just %d", results, related.ID)
	}
	if results[0].Score != 4 || results[0].MatchedTermCount != 2 {
		t.Fatalf("score = %v count = %d, want 4 and 2", results[0].Score, results[0].MatchedTermCount)
	}
	if results[0].WeightedTermCoverage != 1 {
		t.Fatalf("coverage = %v, want 1", results[0].WeightedTermCoverage)
	}
}

func TestPostgresBackendDateFilters(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()

	older := seedPostgresSearchMessage(t, db, user, mailbox, 40, "Terminplan alt", "planung")
	newer := seedPostgresSearchMessage(t, db, user, mailbox, 41, "Terminplan neu", "planung")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: older}, {Message: newer}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	cutoff := time.Now().AddDate(0, -6, 0)
	if _, err := db.DB().ExecContext(ctx, `UPDATE messages SET date_unix = $1 WHERE id = $2`,
		cutoff.AddDate(-1, 0, 0).Unix(), older.ID); err != nil {
		t.Fatal(err)
	}

	query := "terminplan after:" + cutoff.AddDate(0, -1, 0).Format("2006-01-02")
	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, query, 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].ID != newer.ID {
		t.Fatalf("after hits = %v, err = %v, want just %d", hits, err, newer.ID)
	}
}

func TestPostgresBackendFuzzyMatching(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	if err := db.EnsureTrigramSearch(ctx); err != nil {
		t.Fatalf("ensure trigram search: %v", err)
	}
	if !db.TrigramSearchEnabled() {
		t.Fatal("trigram search not enabled after ensure")
	}

	invoice := seedPostgresSearchMessage(t, db, user, mailbox, 50, "Rechnung Februar", "anbei die rechnung")
	exact := seedPostgresSearchMessage(t, db, user, mailbox, 51, "Rehcnung wörtlich", "genau dieser dreher steht hier")
	other := seedPostgresSearchMessage(t, db, user, mailbox, 52, "Kantinenplan", "essen der woche")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: invoice}, {Message: exact}, {Message: other}}); err != nil {
		t.Fatalf("index: %v", err)
	}

	// The transposition typo finds the real word by similarity, and the exact
	// occurrence of the typo itself by lexeme. The exact lexeme match must
	// rank above the similarity match.
	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "rehcnung", 10, 0, SearchOptions{})
	if err != nil {
		t.Fatalf("fuzzy search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("fuzzy hits = %v, want the typo document and the similar one", hits)
	}
	if hits[0].ID != exact.ID || hits[1].ID != invoice.ID {
		t.Fatalf("order = %d, %d; want exact lexeme %d above similarity %d", hits[0].ID, hits[1].ID, exact.ID, invoice.ID)
	}

	// Multi-term queries stay AND-composed: one fuzzy term plus one exact term
	// that only the invoice satisfies.
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rehcnung februar", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].ID != invoice.ID {
		t.Fatalf("fuzzy+exact hits = %v, err = %v, want just %d", hits, err, invoice.ID)
	}

	// Fuzzy off keeps the typo unmatched by similarity.
	off := SearchOptions{Behavior: SearchBehavior{Fuzzy: "off"}}
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rehcnung", 10, 0, off)
	if err != nil || len(hits) != 1 || hits[0].ID != exact.ID {
		t.Fatalf("fuzzy-off hits = %v, err = %v, want just the literal document", hits, err)
	}

	// Short terms stay exact even with fuzzy on: no trigram noise.
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "esen", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 0 {
		t.Fatalf("short-term hits = %v, err = %v, want none", hits, err)
	}

	// Quoted phrases never fuzz.
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, `"rehcnung februar"`, 10, 0, SearchOptions{})
	if err != nil || len(hits) != 0 {
		t.Fatalf("quoted fuzzy hits = %v, err = %v, want none", hits, err)
	}
}

func TestPostgresBackendFuzzyDegradesWithoutTrigram(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	// EnsureTrigramSearch deliberately not called: the flag stays off, and the
	// query path must stay exact instead of failing on a missing operator.
	msg := seedPostgresSearchMessage(t, db, user, mailbox, 60, "Rechnung März", "anbei")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: msg}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "rehcnung", 10, 0, SearchOptions{})
	if err != nil {
		t.Fatalf("search without trigram: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %v, want none without fuzzy", hits)
	}
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rechnung", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("exact hits = %v, err = %v, want 1", hits, err)
	}
}

// TestPostgresBackendSplitsAddressesAndURLs pins the tokenizer agreement: an
// address or link in the body has to be reachable by its parts, which
// PostgreSQL's own parser would keep as one lexeme.
func TestPostgresBackendSplitsAddressesAndURLs(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	msg := seedPostgresSearchMessage(t, db, user, mailbox, 70, "Kontaktdaten",
		"schreib an kontakt@firma-beispiel.de oder https://portal.firma-beispiel.de/anmelden")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: msg}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	for _, query := range []string{"firma", "beispiel", "kontakt", "portal", "anmelden", "kontakt@firma-beispiel.de"} {
		hits, err := svc.SearchHitsWithOptions(ctx, user.ID, query, 10, 0, SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if len(hits) != 1 || hits[0].ID != msg.ID {
			t.Errorf("query %q found %v, want the message with the address and link", query, hits)
		}
	}
}

func TestPGIndexTextBoundsAndSplits(t *testing.T) {
	long := strings.Repeat("x", 5000)
	out := pgIndexText(long, pgMaxBodyBytes)
	for _, run := range strings.Fields(out) {
		if len(run) > pgMaxLexemeBytes {
			t.Fatalf("run of %d bytes exceeds the per-lexeme ceiling", len(run))
		}
	}
	if strings.ReplaceAll(out, " ", "") != long {
		t.Fatal("splitting an oversized run lost or changed content")
	}
	if got := pgIndexText("Rechnung/2026 Nr. 7", 0); got != "" {
		t.Fatalf("zero budget returned %q", got)
	}
	if got := pgIndexText("Ärger mit Ümlauten", 5); len(got) > 5 {
		t.Fatalf("bounded text is %d bytes, want at most 5", len(got))
	}
}

// TestPostgresBackendVectorStaysUnderLimit feeds the worst shape PostgreSQL
// rejects - a message of nothing but distinct short words - and checks that
// the projection keeps it insertable. Before the combined budget this failed
// permanently: the repair path re-projects the same text and hits the same
// hard limit forever.
func TestPostgresBackendVectorStaysUnderLimit(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()

	var body strings.Builder
	for i := 0; body.Len() < 3*pgMaxVectorInputBytes; i++ {
		fmt.Fprintf(&body, "a%d ", i)
	}
	huge := seedPostgresSearchMessage(t, db, user, mailbox, 80, "Riesige Nachricht", body.String())
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: huge,
		Attachments: []AttachmentDoc{{Filename: "gross.txt", ContentType: "text/plain", Text: body.String()}}},
	}); err != nil {
		t.Fatalf("indexing an oversized message failed: %v", err)
	}
	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "riesige", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].ID != huge.ID {
		t.Fatalf("hits = %v, err = %v; the subject must survive the budget", hits, err)
	}
}

func TestPostgresBackendFoldsCaseBeyondASCII(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	msg := seedPostgresSearchMessage(t, db, user, mailbox, 90, "ÜBERWEISUNG ausgeführt", "der betrag ist raus")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: msg}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE messages SET from_addr = 'Jörg Müller <joerg@beispiel.test>' WHERE id = $1`, msg.ID); err != nil {
		t.Fatal(err)
	}

	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "betrag subject:überweisung", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 || hits[0].ID != msg.ID {
		t.Fatalf("subject filter hits = %v, err = %v; lowercase query must match the uppercase subject", hits, err)
	}
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "betrag from:JÖRG", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("from filter hits = %v, err = %v; uppercase query must match the mixed-case sender", hits, err)
	}
	// The boost path lowercases app-side and compares against the column.
	opts := SearchOptions{SenderBoosts: []SenderBoost{{Sender: "Jörg Müller", Boost: 5}}}
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "betrag", 10, 0, opts)
	if err != nil || len(hits) != 1 {
		t.Fatalf("boosted hits = %v, err = %v", hits, err)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("score = %v, want the non-ASCII sender boost applied", hits[0].Score)
	}
}

// TestPostgresBackendExplainWalksLargeThreads covers the id list a big thread
// hands to explain: the store caps one restriction at 500, and the Bleve path
// it replaces had no cap at all.
func TestPostgresBackendExplainWalksLargeThreads(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()

	ids := make([]int64, 0, 640)
	var target int64
	docs := make([]MessageIndexDocument, 0, 640)
	for i := 0; i < 640; i++ {
		subject := "Sammelthread"
		if i == 600 {
			subject = "Sammelthread Schlüsselwort"
		}
		msg := seedPostgresSearchMessage(t, db, user, mailbox, uint32(1000+i), subject, "rumpf")
		if i == 600 {
			target = msg.ID
		}
		ids = append(ids, msg.ID)
		docs = append(docs, MessageIndexDocument{Message: msg})
	}
	if err := svc.IndexMessages(ctx, docs); err != nil {
		t.Fatalf("index: %v", err)
	}

	result, ok, err := svc.ExplainMessagesWithOptions(ctx, user.ID, ids, "schlüsselwort", SearchOptions{})
	if err != nil {
		t.Fatalf("explain over %d ids: %v", len(ids), err)
	}
	if !ok || result.ID != target {
		t.Fatalf("explain = %+v, ok = %v; want the matching message %d from beyond the first chunk", result, ok, target)
	}
}

// TestPostgresBackendSearchWithOptions covers the id-only entry point, which
// list building uses and which never reached the postgres branch.
func TestPostgresBackendSearchWithOptions(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	msg := seedPostgresSearchMessage(t, db, user, mailbox, 95, "Listenaufbau", "inhalt")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: msg}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	ids, err := svc.Search(ctx, user.ID, "listenaufbau", 10, 0)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(ids) != 1 || ids[0] != msg.ID {
		t.Fatalf("ids = %v, want [%d]", ids, msg.ID)
	}
}

// TestPostgresBackendFuzzyHitNamesFields covers the reporting gap a fuzzy-only
// match leaves: the weight-class columns answer for the lexeme query, so a row
// that came in through similarity alone matches none of them, and attachment
// snippets read these names.
func TestPostgresBackendFuzzyHitNamesFields(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	if err := db.EnsureTrigramSearch(ctx); err != nil {
		t.Fatalf("ensure trigram search: %v", err)
	}
	msg := seedPostgresSearchMessage(t, db, user, mailbox, 110, "Zahlungserinnerung", "bitte um ausgleich")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: msg}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "zahlungserinerung", 10, 0, SearchOptions{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("fuzzy hits = %v, err = %v, want 1", hits, err)
	}
	if len(hits[0].Fields) == 0 {
		t.Fatal("a fuzzy-only hit reported no matched fields")
	}
}

func TestPostgresBackendSimilarityIsTenantScoped(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	other, err := db.CreateUser(ctx, "pg-similar-other@example.test", "Other", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	current := seedPostgresSearchMessage(t, db, user, mailbox, 120, "Wartungsfenster", "wartung")
	related := seedPostgresSearchMessage(t, db, user, mailbox, 121, "Wartungsfenster Rückfrage", "wartung")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: current}, {Message: related}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	request := plugins.SimilarMessagesRequest{
		CandidateMessageIDs: []int64{related.ID},
		Terms:               []plugins.SimilarityTerm{{Field: plugins.SimilarityFieldSubject, Text: "Wartungsfenster", Weight: 2}},
		Limit:               5,
	}
	results, err := svc.SimilarMessages(ctx, db, other.ID, request)
	if err != nil {
		t.Fatalf("cross-tenant similarity: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("another tenant saw %d similar messages", len(results))
	}
	if _, ok, err := svc.ExplainMessagesWithOptions(ctx, other.ID, []int64{related.ID}, "wartungsfenster", SearchOptions{}); err != nil || ok {
		t.Fatalf("another tenant explained a foreign message, ok = %v, err = %v", ok, err)
	}
}

// TestPostgresBackendSimilarityMatchesAnyWord pins the semantics the Bleve
// backend has: a multi-word subject or body term matches on any of its words.
func TestPostgresBackendSimilarityMatchesAnyWord(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	candidate := seedPostgresSearchMessage(t, db, user, mailbox, 130, "Serverwartung", "nur ein wort trifft")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: candidate}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	results, err := svc.SimilarMessages(ctx, db, user.ID, plugins.SimilarMessagesRequest{
		CandidateMessageIDs: []int64{candidate.ID},
		Terms:               []plugins.SimilarityTerm{{Field: plugins.SimilarityFieldSubject, Text: "Serverwartung Kantinenplan", Weight: 2}},
		Limit:               5,
	})
	if err != nil {
		t.Fatalf("similar: %v", err)
	}
	if len(results) != 1 || results[0].MessageID != candidate.ID {
		t.Fatalf("results = %+v; a term's words must match individually", results)
	}
}

// TestPostgresBackendPurgeReportsProgressInBatches covers the bound the Bleve
// purge keeps: short commits, one callback per batch, and a caller-side abort
// that stops the purge part way.
func TestPostgresBackendPurgeReportsProgressInBatches(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	docs := make([]MessageIndexDocument, 0, 250)
	for i := 0; i < 250; i++ {
		msg := seedPostgresSearchMessage(t, db, user, mailbox, uint32(2000+i), "Aufräumen", "inhalt")
		docs = append(docs, MessageIndexDocument{Message: msg})
	}
	if err := svc.IndexMessages(ctx, docs); err != nil {
		t.Fatalf("index: %v", err)
	}

	var batches []int
	purged, err := svc.PurgeMailboxWithProgress(ctx, user.ID, mailbox.ID, func(n int) error {
		batches = append(batches, n)
		return nil
	})
	if err != nil || purged != 250 {
		t.Fatalf("purged = %d, err = %v, want 250", purged, err)
	}
	if len(batches) < 3 {
		t.Fatalf("progress batches = %v, want several bounded batches", batches)
	}
	if count, err := svc.CountMailboxMessages(ctx, user.ID, mailbox.ID); err != nil || count != 0 {
		t.Fatalf("count after purge = %d, err = %v", count, err)
	}
}

func TestPostgresBackendPurgeStopsOnCallbackError(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	docs := make([]MessageIndexDocument, 0, 250)
	for i := 0; i < 250; i++ {
		msg := seedPostgresSearchMessage(t, db, user, mailbox, uint32(3000+i), "Abbruch", "inhalt")
		docs = append(docs, MessageIndexDocument{Message: msg})
	}
	if err := svc.IndexMessages(ctx, docs); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := svc.PurgeMailboxWithProgress(ctx, user.ID, mailbox.ID, func(int) error {
		return context.Canceled
	}); err == nil {
		t.Fatal("an aborting callback did not stop the purge")
	}
	count, err := svc.CountMailboxMessages(ctx, user.ID, mailbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 || count == 250 {
		t.Fatalf("count = %d; an abort must stop part way", count)
	}
}

// Typo tolerance is a fallback, not a co-equal branch: it reads a second copy
// of every candidate's text, so it runs for the query that found nothing and
// stays out of the way of the one that found a page of mail. The near-miss
// document is the witness - it can only be reached by similarity.
func TestPostgresSearchFuzzesOnlyWhileExactMatchesAreScarce(t *testing.T) {
	svc, db, user, mailbox := openPostgresSearchFixtures(t)
	ctx := context.Background()
	if err := db.EnsureTrigramSearch(ctx); err != nil {
		t.Fatalf("ensure trigram search: %v", err)
	}

	// A neighbour whose mailbox is full of the same word. The gate counts
	// matches to decide whether this tenant's query needs typo tolerance, and
	// counting theirs would answer one reader's question with another's mail.
	neighbour, neighbourInbox := newPostgresSearchTenant(t, db, "pg-search-neighbour@example.test")
	crowd := make([]MessageIndexDocument, 0, pgFuzzyFallbackBelow*2)
	for uid := uint32(5000); uid < 5000+uint32(pgFuzzyFallbackBelow)*2; uid++ {
		crowd = append(crowd, MessageIndexDocument{
			Message: seedPostgresSearchMessage(t, db, neighbour, neighbourInbox, uid, "Rechnung", "anbei die rechnung"),
		})
	}
	if err := svc.IndexMessages(ctx, crowd); err != nil {
		t.Fatalf("index neighbour: %v", err)
	}

	nearMiss := seedPostgresSearchMessage(t, db, user, mailbox, 900, "Rechnnung Dreher", "nur ueber aehnlichkeit erreichbar")
	docs := []MessageIndexDocument{{Message: nearMiss}}
	// One short of the gate: the query is still answered with typo tolerance.
	for uid := uint32(901); uid < 901+uint32(pgFuzzyFallbackBelow)-1; uid++ {
		docs = append(docs, MessageIndexDocument{
			Message: seedPostgresSearchMessage(t, db, user, mailbox, uid, "Rechnung", "anbei die rechnung"),
		})
	}
	if err := svc.IndexMessages(ctx, docs); err != nil {
		t.Fatalf("index: %v", err)
	}

	hits, err := svc.SearchHitsWithOptions(ctx, user.ID, "rechnung", 200, 0, SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != pgFuzzyFallbackBelow {
		t.Fatalf("hits = %d, want the %d exact matches plus the near miss - the neighbour's mail must not appear or count",
			len(hits), pgFuzzyFallbackBelow)
	}
	if !hitsContain(hits, nearMiss.ID) {
		t.Fatalf("hits = %v, want the near miss %d reached by similarity one match short of the gate", hits, nearMiss.ID)
	}

	// One more exact match puts the query exactly on the gate, which is where
	// it closes: the near miss no longer has a way in. Testing the boundary
	// itself is the point - a gate written one off would pass either side of it.
	onTheGate := seedPostgresSearchMessage(t, db, user, mailbox, 900+uint32(pgFuzzyFallbackBelow), "Rechnung", "anbei die rechnung")
	if err := svc.IndexMessages(ctx, []MessageIndexDocument{{Message: onTheGate}}); err != nil {
		t.Fatalf("index: %v", err)
	}
	hits, err = svc.SearchHitsWithOptions(ctx, user.ID, "rechnung", 200, 0, SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != pgFuzzyFallbackBelow {
		t.Fatalf("hits = %d, want exactly the %d exact matches", len(hits), pgFuzzyFallbackBelow)
	}
	if hitsContain(hits, nearMiss.ID) {
		t.Fatalf("near miss %d still matched with exactly %d exact hits, so the gate is off by one",
			nearMiss.ID, pgFuzzyFallbackBelow)
	}

	// The ranked query cuts the page in an inner layer and answers the
	// weight-class question in an outer one. Paging has to survive that.
	first, err := svc.SearchHitsWithOptions(ctx, user.ID, "rechnung", 10, 0, SearchOptions{})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	second, err := svc.SearchHitsWithOptions(ctx, user.ID, "rechnung", 10, 10, SearchOptions{})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(first) != 10 || len(second) != 10 {
		t.Fatalf("pages = %d and %d, want 10 each", len(first), len(second))
	}
	for _, hit := range second {
		if hitsContain(first, hit.ID) {
			t.Fatalf("message %d appears on both pages", hit.ID)
		}
	}
	for _, hit := range first {
		if len(hit.Fields) == 0 {
			t.Fatalf("hit %d reports no matched field, so the deferred class query lost its answer", hit.ID)
		}
	}
}

func hitsContain(hits []Hit, id int64) bool {
	for _, hit := range hits {
		if hit.ID == id {
			return true
		}
	}
	return false
}
