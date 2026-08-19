package search

import (
	"context"
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
