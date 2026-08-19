// File overview: The query side of full-text search on PostgreSQL. The search
// package parses the user's query and normalizes the ranking knobs; this file
// turns the neutral spec into one SQL statement over message_search joined
// with messages, so every filter reads the current row — flags, mailbox, and
// dates are never stale the way an index copy is. Ranking happens in the same
// statement: ts_rank_cd over the weighted vector, plus the sender-history and
// recency nudges the Bleve query encoded as boolean should-clauses and a
// custom scorer.

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// maxMessageSearchTextTerms bounds one query's term lists. Each term adds a
// condition and up to two bind parameters, and every one of them costs a
// to_tsquery evaluation per candidate row, so an unbounded query is both a
// parameter-limit hazard and a way to make one request expensive. The caller
// trims to a smaller working limit; this is the boundary that refuses rather
// than trusts.
const maxMessageSearchTextTerms = 64

// collateDefault forces case handling that covers more than ASCII. The text
// columns are declared COLLATE "C" (the baseline's translation of SQLite's
// BINARY), under which lower() and ILIKE fold A-Z and leave every other
// alphabet alone: subject:überweisung would miss "ÜBERWEISUNG", and a sender
// boost for a non-ASCII address would never fire. The database's own collation
// does the full mapping, and none of these predicates can use an index anyway
// - they filter rows the GIN scan already selected.
const collateDefault = ` COLLATE "default"`

// MessageSearchBoost adds to a hit's score when the sender matches. Pattern is
// compared as a lowercase substring of from_addr, which is how the Bleve
// should-clause on the from field behaved for addresses.
type MessageSearchBoost struct {
	Pattern string
	Boost   float64
}

// MessageSearchRecencyBucket adds Boost while the message is at most MaxAge
// seconds old. Buckets are checked in order; the first match wins.
type MessageSearchRecencyBucket struct {
	MaxAgeSeconds int64
	Boost         float64
}

// MessageSearchTextTerm is one free-text term when fuzzy matching is on: the
// term's own lexeme query, plus the raw term to probe against the word list
// when FuzzyTerm is set (short terms keep it empty and stay exact).
type MessageSearchTextTerm struct {
	TSQuery   string
	FuzzyTerm string
}

// MessageSearchQuery is the neutral spec the search package compiles a parsed
// query into. Zero values mean "no constraint" throughout.
type MessageSearchQuery struct {
	UserID int64
	// TSQuery is to_tsquery('simple', ...) input. Empty means no text
	// constraint: filters alone select, and ranking is boosts plus recency.
	// With TextTerms set it only ranks and reports weight classes; the
	// per-term conditions decide membership.
	TSQuery string
	// TextTerms replaces the single tsquery condition when fuzzy matching is
	// on: a message matches when every term matches exactly or by similarity.
	TextTerms []MessageSearchTextTerm
	// FuzzyThreshold is the pg_trgm word-similarity floor for this query,
	// applied with SET LOCAL inside the query's transaction. Required when any
	// TextTerms carry a FuzzyTerm.
	FuzzyThreshold float64
	// NotTSQueries exclude matches, one per negated term.
	NotTSQueries []string
	// Weights orders {D, C, B, A} for ts_rank_cd, matching PostgreSQL's
	// weight-array convention.
	Weights [4]float64

	HasAttachment *bool
	IsRead        *bool
	IsStarred     *bool
	IsEncrypted   *bool
	IsSigned      *bool
	Language      string
	// The four field filters are ILIKE patterns including their wildcards.
	FromPattern     string
	ToPattern       string
	CCPattern       string
	SubjectPattern  string
	FilenamePattern string
	// AfterUnix is inclusive, BeforeUnix exclusive, both 0 when open — the
	// same interval the Bleve date range used.
	AfterUnix  int64
	BeforeUnix int64

	SenderBoosts   []MessageSearchBoost
	RecencyBuckets []MessageSearchRecencyBucket
	// NowUnix anchors the recency buckets; passed in so tests can pin it.
	NowUnix int64

	// MessageIDs restricts the search to the given messages (match/explain).
	MessageIDs []int64

	Limit  int
	Offset int
}

// MessageSearchHit is one ranked row. The four matched flags report which
// weight classes of the vector the text query matched — subject (A),
// addresses (B), body (C), attachments (D) — for highlighting and explain.
type MessageSearchHit struct {
	MessageID int64
	Score     float64
	MatchedA  bool
	MatchedB  bool
	MatchedC  bool
	MatchedD  bool
}

// SearchMessageIDs runs one ranked search. Restricted throughout to the
// tenant on both tables, so a joined row can never cross users.
func (s *Store) SearchMessageIDs(ctx context.Context, q MessageSearchQuery) ([]MessageSearchHit, error) {
	if q.UserID <= 0 {
		return nil, fmt.Errorf("user id must be positive")
	}
	db, err := s.dataDB(ctx, q.UserID)
	if err != nil {
		return nil, err
	}
	hasText := strings.TrimSpace(q.TSQuery) != ""

	if len(q.TextTerms) > maxMessageSearchTextTerms {
		return nil, fmt.Errorf("a search may carry at most %d text terms, got %d", maxMessageSearchTextTerms, len(q.TextTerms))
	}
	if len(q.NotTSQueries) > maxMessageSearchTextTerms {
		return nil, fmt.Errorf("a search may carry at most %d negated terms, got %d", maxMessageSearchTextTerms, len(q.NotTSQueries))
	}
	fuzzy := false
	for _, term := range q.TextTerms {
		if term.FuzzyTerm != "" {
			fuzzy = true
			break
		}
	}
	if fuzzy && !(q.FuzzyThreshold > 0 && q.FuzzyThreshold <= 1) {
		return nil, fmt.Errorf("fuzzy terms need a similarity threshold in (0, 1], got %g", q.FuzzyThreshold)
	}

	var score strings.Builder
	var args []any
	if hasText {
		score.WriteString(`ts_rank_cd(?::float4[], ms.tsv, to_tsquery('simple', ?))`)
		args = append(args, formatWeightArray(q.Weights), q.TSQuery)
	} else {
		score.WriteString(`0::float4`)
	}
	if fuzzy {
		// Rank fuzzy evidence below exact lexeme rank: similarity tops out at
		// 0.3 per term, so an exact subject hit always outranks a typo match.
		for _, term := range q.TextTerms {
			if term.FuzzyTerm == "" {
				continue
			}
			score.WriteString(` + (0.3 * word_similarity(?, ms.words))::float4`)
			args = append(args, term.FuzzyTerm)
		}
	}
	for _, boost := range q.SenderBoosts {
		pattern := strings.ToLower(strings.TrimSpace(boost.Pattern))
		if pattern == "" || boost.Boost <= 0 {
			continue
		}
		score.WriteString(` + CASE WHEN position(? in lower(m.from_addr` + collateDefault + `)) > 0 THEN ?::float4 ELSE 0 END`)
		args = append(args, pattern, boost.Boost)
	}
	if len(q.RecencyBuckets) > 0 && q.NowUnix > 0 {
		score.WriteString(` + CASE`)
		for _, bucket := range q.RecencyBuckets {
			score.WriteString(` WHEN (? - m.date_unix) <= ? THEN ?::float4`)
			args = append(args, q.NowUnix, bucket.MaxAgeSeconds, bucket.Boost)
		}
		score.WriteString(` ELSE 0 END`)
	}

	var matches string
	if hasText {
		matches = `ts_filter(ms.tsv, '{a}'::"char"[]) @@ to_tsquery('simple', ?),
			ts_filter(ms.tsv, '{b}'::"char"[]) @@ to_tsquery('simple', ?),
			ts_filter(ms.tsv, '{c}'::"char"[]) @@ to_tsquery('simple', ?),
			ts_filter(ms.tsv, '{d}'::"char"[]) @@ to_tsquery('simple', ?)`
		args = append(args, q.TSQuery, q.TSQuery, q.TSQuery, q.TSQuery)
	} else {
		matches = `false, false, false, false`
	}

	conditions := []string{"ms.user_id = ?", "m.user_id = ?"}
	args = append(args, q.UserID, q.UserID)
	switch {
	case len(q.TextTerms) > 0:
		for _, term := range q.TextTerms {
			if strings.TrimSpace(term.TSQuery) == "" {
				continue
			}
			if term.FuzzyTerm != "" {
				conditions = append(conditions, `(ms.tsv @@ to_tsquery('simple', ?) OR ? <% ms.words)`)
				args = append(args, term.TSQuery, term.FuzzyTerm)
				continue
			}
			conditions = append(conditions, `ms.tsv @@ to_tsquery('simple', ?)`)
			args = append(args, term.TSQuery)
		}
	case hasText:
		conditions = append(conditions, `ms.tsv @@ to_tsquery('simple', ?)`)
		args = append(args, q.TSQuery)
	}
	for _, not := range q.NotTSQueries {
		if strings.TrimSpace(not) == "" {
			continue
		}
		conditions = append(conditions, `NOT (ms.tsv @@ to_tsquery('simple', ?))`)
		args = append(args, not)
	}
	for _, flag := range []struct {
		column string
		value  *bool
	}{
		{"m.has_attachments", q.HasAttachment},
		{"m.is_read", q.IsRead},
		{"m.is_starred", q.IsStarred},
		{"m.is_encrypted", q.IsEncrypted},
		{"m.is_signed", q.IsSigned},
	} {
		if flag.value == nil {
			continue
		}
		want := 0
		if *flag.value {
			want = 1
		}
		conditions = append(conditions, flag.column+" = ?")
		args = append(args, want)
	}
	if q.Language != "" {
		// Language codes are ASCII by definition, so this one needs no
		// collation override.
		conditions = append(conditions, `lower(m.language_code) = lower(?)`)
		args = append(args, q.Language)
	}
	for _, pattern := range []struct {
		column string
		value  string
	}{
		{"m.from_addr", q.FromPattern},
		{"m.to_addr", q.ToPattern},
		{"m.cc_addr", q.CCPattern},
		{"m.subject", q.SubjectPattern},
	} {
		if pattern.value == "" {
			continue
		}
		conditions = append(conditions, pattern.column+collateDefault+" ILIKE ?")
		args = append(args, pattern.value)
	}
	if q.FilenamePattern != "" {
		conditions = append(conditions, `EXISTS (SELECT 1 FROM attachments a
			WHERE a.user_id = ms.user_id AND a.message_id = ms.message_id AND a.filename`+collateDefault+` ILIKE ?)`)
		args = append(args, q.FilenamePattern)
	}
	if q.AfterUnix > 0 {
		conditions = append(conditions, "m.date_unix >= ?")
		args = append(args, q.AfterUnix)
	}
	if q.BeforeUnix > 0 {
		conditions = append(conditions, "m.date_unix < ?")
		args = append(args, q.BeforeUnix)
	}
	if len(q.MessageIDs) > 0 {
		if len(q.MessageIDs) > 500 {
			return nil, fmt.Errorf("message id restriction is limited to 500 ids, got %d", len(q.MessageIDs))
		}
		for _, id := range q.MessageIDs {
			args = append(args, id)
		}
		conditions = append(conditions, "ms.message_id IN ("+sqlPlaceholders(len(q.MessageIDs))+")")
	}

	limit := q.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset := max(q.Offset, 0)
	args = append(args, limit, offset)

	query := `SELECT ms.message_id, (` + score.String() + `)::float8 AS score, ` + matches + `
		FROM message_search ms
		JOIN messages m ON m.id = ms.message_id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY score DESC, m.date_unix DESC, ms.message_id DESC
		LIMIT ? OFFSET ?`
	var rows *sql.Rows
	if fuzzy {
		// The <% operator reads its floor from a GUC, and the useful floor for
		// typo matching (~0.35) is far below the server default (0.6). SET
		// LOCAL scopes the change to this transaction; the value is a bounded
		// number this function validated, rendered as a literal because SET
		// takes no parameters.
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`SET LOCAL pg_trgm.word_similarity_threshold = %.2f`, q.FuzzyThreshold)); err != nil {
			return nil, err
		}
		rows, err = tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		rows, err = db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()
	var hits []MessageSearchHit
	for rows.Next() {
		var hit MessageSearchHit
		if err := rows.Scan(&hit.MessageID, &hit.Score, &hit.MatchedA, &hit.MatchedB, &hit.MatchedC, &hit.MatchedD); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// formatWeightArray renders the {D,C,B,A} weights as a float4[] literal.
func formatWeightArray(weights [4]float64) string {
	parts := make([]string, len(weights))
	for i, w := range weights {
		parts[i] = strings.TrimSpace(fmt.Sprintf("%g", w))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// MessageSearchTermProbe asks whether one term matches a message's vector,
// optionally restricted to a weight class ("a" subject, "b" addresses, "c"
// body, "d" attachments; empty probes the whole vector). Similarity scoring
// runs these against a bounded candidate set.
type MessageSearchTermProbe struct {
	TSQuery     string
	WeightClass string
}

// ProbeMessageSearchTerms reports, per candidate message, which probes match.
// The result maps message id to a probe-indexed slice.
func (s *Store) ProbeMessageSearchTerms(ctx context.Context, userID int64, messageIDs []int64, probes []MessageSearchTermProbe) (map[int64][]bool, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user id must be positive")
	}
	if len(messageIDs) == 0 || len(probes) == 0 {
		return map[int64][]bool{}, nil
	}
	if len(probes) > 64 {
		return nil, fmt.Errorf("term probes are limited to 64, got %d", len(probes))
	}
	db, err := s.dataDB(ctx, userID)
	if err != nil {
		return nil, err
	}
	var columns strings.Builder
	probeArgs := make([]any, 0, len(probes))
	for i, probe := range probes {
		columns.WriteString(", ")
		switch probe.WeightClass {
		case "":
			columns.WriteString(`ms.tsv @@ to_tsquery('simple', ?)`)
		case "a", "b", "c", "d":
			columns.WriteString(`ts_filter(ms.tsv, '{` + probe.WeightClass + `}'::"char"[]) @@ to_tsquery('simple', ?)`)
		default:
			return nil, fmt.Errorf("unknown weight class %q in probe %d", probe.WeightClass, i)
		}
		probeArgs = append(probeArgs, probe.TSQuery)
	}
	out := make(map[int64][]bool, len(messageIDs))
	for start := 0; start < len(messageIDs); start += 500 {
		end := min(start+500, len(messageIDs))
		chunk := messageIDs[start:end]
		args := make([]any, 0, len(probeArgs)+1+len(chunk))
		args = append(args, probeArgs...)
		args = append(args, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `SELECT ms.message_id`+columns.String()+`
			FROM message_search ms
			WHERE ms.user_id = ? AND ms.message_id IN (`+sqlPlaceholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		scan := make([]any, 1+len(probes))
		for rows.Next() {
			var id int64
			matched := make([]bool, len(probes))
			scan[0] = &id
			for i := range matched {
				scan[i+1] = &matched[i]
			}
			if err := rows.Scan(scan...); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = matched
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}
