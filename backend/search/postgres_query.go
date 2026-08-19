// File overview: The read path of the PostgreSQL search backend. parseQuery
// stays the one grammar for both backends; this file compiles its result into
// the store's neutral query spec — filters onto the joined messages row, free
// text into a tsquery, the ranking knobs into the weight array, sender-boost
// cases and recency buckets. Field/term reporting comes back as weight-class
// matches (subject/addresses/body/attachments), which is coarser than Bleve's
// per-term locations but carries the same highlighting decisions.
//
// Known reductions against the Bleve path, recorded in
// docs/search-postgres-plan.md §5: no fuzzy matching yet (pg_trgm is a later
// phase), query-side compound splitting relies on the split terms already in
// the vector, and Explain reports weight-class matches instead of a scorer
// tree.

package search

import (
	"context"
	"fmt"
	"strings"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

// pgWeights orders {D, C, B, A}: attachments, body, addresses, subject — the
// same precedence the Bleve field boosts encode, normalized to ts_rank_cd's
// convention. The attachment weight scales with the user's knob.
func pgWeights(behavior normalizedSearchBehavior) [4]float64 {
	return [4]float64{0.1 * behavior.attachmentBoostScale(), 0.2, 0.5, 1.0}
}

// pgTSQuery renders one text as to_tsquery('simple', ...) input: terms from
// the same normalization the needle extraction uses, AND-joined, quoted
// phrases as adjacency, and a prefix match on the final term of a free query
// so as-you-type search sees partial words.
func pgTSQuery(text string, quoted, prefixLast bool) string {
	terms := strings.Fields(normalizeSearchText(text))
	if len(terms) == 0 {
		return ""
	}
	quotedTerms := make([]string, len(terms))
	for i, term := range terms {
		quotedTerms[i] = "'" + strings.ReplaceAll(term, "'", "''") + "'"
	}
	if quoted && len(quotedTerms) > 1 {
		return strings.Join(quotedTerms, " <-> ")
	}
	if prefixLast {
		quotedTerms[len(quotedTerms)-1] += ":*"
	}
	return strings.Join(quotedTerms, " & ")
}

// pgSearchSpec compiles a parsed query and the ranking options into the
// store's spec. Shared by search, match and explain so the three stay one
// grammar with one scoring.
func pgSearchSpec(userID int64, parsed parsedQuery, opts SearchOptions, limit, offset int) store.MessageSearchQuery {
	behavior := opts.Behavior.normalized()
	spec := store.MessageSearchQuery{
		UserID:        userID,
		TSQuery:       pgTSQuery(parsed.Text, parsed.TextQuoted, !parsed.TextQuoted),
		Weights:       pgWeights(behavior),
		HasAttachment: parsed.HasAttachment,
		IsRead:        parsed.IsRead,
		IsStarred:     parsed.IsStarred,
		IsEncrypted:   parsed.IsEncrypted,
		IsSigned:      parsed.IsSigned,
		Language:      parsed.Language,
		Limit:         limit,
		Offset:        offset,
		NowUnix:       time.Now().UTC().Unix(),
	}
	for _, negated := range parsed.NegatedText {
		if q := pgTSQuery(negated.Text, negated.Quoted, false); q != "" {
			spec.NotTSQueries = append(spec.NotTSQueries, q)
		}
	}
	pattern := func(value string) string {
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if value == "" {
			return ""
		}
		return "%" + escapeLikePattern(value) + "%"
	}
	spec.FromPattern = pattern(parsed.From)
	spec.ToPattern = pattern(parsed.To)
	spec.CCPattern = pattern(parsed.CC)
	spec.SubjectPattern = pattern(parsed.Subject)
	spec.FilenamePattern = pattern(parsed.Filename)
	if !parsed.After.IsZero() {
		spec.AfterUnix = parsed.After.UTC().Unix()
	}
	if !parsed.Before.IsZero() {
		spec.BeforeUnix = parsed.Before.UTC().Unix()
	}
	appendBoosts := func(boosts []SenderBoost, scale float64) {
		if scale <= 0 {
			return
		}
		for _, boost := range boosts {
			if len(spec.SenderBoosts) >= 40 {
				return
			}
			sender := strings.TrimSpace(boost.Sender)
			if sender == "" || boost.Boost <= 0 {
				continue
			}
			spec.SenderBoosts = append(spec.SenderBoosts, store.MessageSearchBoost{
				Pattern: strings.ToLower(sender), Boost: boost.Boost * scale,
			})
		}
	}
	appendBoosts(opts.SenderBoosts, behavior.SenderBoostScale)
	appendBoosts(opts.ContactBoosts, behavior.ContactBoostScale)
	// The Bleve path only nudges by recency when no explicit date range says
	// the user is already navigating time; mirrored here.
	if parsed.After.IsZero() && parsed.Before.IsZero() {
		for _, bucket := range RecencyRankBuckets(behavior.RecencyBias) {
			spec.RecencyBuckets = append(spec.RecencyBuckets, store.MessageSearchRecencyBucket{
				MaxAgeSeconds: int64(bucket.Age / time.Second), Boost: bucket.Boost,
			})
		}
	}
	return spec
}

// escapeLikePattern neutralizes LIKE wildcards in user text; the pattern's own
// wildcards are added around the escaped body.
func escapeLikePattern(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

// pgMatchedFields translates weight-class matches into the field names the
// explain panel and the highlighter already understand.
func pgMatchedFields(hit store.MessageSearchHit) []string {
	var fields []string
	if hit.MatchedA {
		fields = append(fields, "subject")
	}
	if hit.MatchedB {
		fields = append(fields, "from")
	}
	if hit.MatchedC {
		fields = append(fields, "body")
	}
	if hit.MatchedD {
		fields = append(fields, "attachments")
	}
	return fields
}

func pgNeedleTerms(parsed parsedQuery) []string {
	needles := queryNeedles(parsed)
	terms := make([]string, 0, len(needles))
	for _, needle := range needles {
		terms = append(terms, needle.Term)
	}
	return terms
}

func (s *Service) pgSearchHits(ctx context.Context, userID int64, queryText string, limit, offset int, opts SearchOptions) ([]Hit, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset = max(offset, 0)
	parsed := parseQuery(queryText)
	spec := pgSearchSpec(userID, parsed, opts, limit, offset)
	rows, err := s.pg.SearchMessageIDs(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("postgres search: %w", err)
	}
	terms := pgNeedleTerms(parsed)
	hits := make([]Hit, 0, len(rows))
	for _, row := range rows {
		hits = append(hits, Hit{
			ID:         row.MessageID,
			Score:      row.Score,
			Terms:      terms,
			Fields:     pgMatchedFields(row),
			QueryTerms: terms,
		})
	}
	return hits, nil
}

func (s *Service) pgExplainMessageIDs(ctx context.Context, userID int64, messageIDs []int64, queryText string, opts SearchOptions) (ExplanationResult, bool, error) {
	parsed := parseQuery(queryText)
	spec := pgSearchSpec(userID, parsed, opts, 1, 0)
	ids := make([]int64, 0, len(messageIDs))
	for _, id := range messageIDs {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return ExplanationResult{}, false, nil
	}
	spec.MessageIDs = ids
	rows, err := s.pg.SearchMessageIDs(ctx, spec)
	if err != nil {
		return ExplanationResult{}, false, fmt.Errorf("postgres match: %w", err)
	}
	if len(rows) == 0 {
		return ExplanationResult{}, false, nil
	}
	row := rows[0]
	terms := pgNeedleTerms(parsed)
	fields := pgMatchedFields(row)
	matches := make([]FieldTermMatch, 0, len(fields))
	for _, field := range fields {
		matches = append(matches, FieldTermMatch{Field: field, Terms: terms})
	}
	return ExplanationResult{
		ID:           row.MessageID,
		Score:        row.Score,
		Terms:        terms,
		Fields:       fields,
		QueryTerms:   terms,
		FieldMatches: matches,
	}, true, nil
}

// pgSimilarityClass maps a similarity term's field onto the weight class its
// text was indexed under.
func pgSimilarityClass(field string) string {
	switch field {
	case plugins.SimilarityFieldSubject:
		return "a"
	case plugins.SimilarityFieldFromDomain:
		return "b"
	case plugins.SimilarityFieldBody:
		return "c"
	default:
		return ""
	}
}

// pgSearchSimilarMessageIDs scores the candidates by probing each weighted
// term against its weight class, mirroring what the Bleve disjunction scored
// with per-term boosts: matched weight becomes score, coverage is the matched
// share of the total weight.
func (s *Service) pgSearchSimilarMessageIDs(ctx context.Context, userID int64, candidateIDs []int64, terms []normalizedSimilarityTerm, limit int) ([]similarityHit, error) {
	probes := make([]store.MessageSearchTermProbe, 0, len(terms))
	for _, term := range terms {
		tsq := pgTSQuery(term.text, false, false)
		if tsq == "" {
			continue
		}
		probes = append(probes, store.MessageSearchTermProbe{TSQuery: tsq, WeightClass: pgSimilarityClass(term.field)})
	}
	if len(probes) == 0 {
		return nil, nil
	}
	matched, err := s.pg.ProbeMessageSearchTerms(ctx, userID, candidateIDs, probes)
	if err != nil {
		return nil, fmt.Errorf("postgres similarity: %w", err)
	}
	var totalWeight float64
	for _, term := range terms {
		totalWeight += term.weight
	}
	hits := make([]similarityHit, 0, len(matched))
	for id, flags := range matched {
		var score, matchedWeight float64
		var matchedTerms, matchedFields []string
		fieldSeen := map[string]bool{}
		count := 0
		probeIndex := 0
		for _, term := range terms {
			if pgTSQuery(term.text, false, false) == "" {
				continue
			}
			if probeIndex < len(flags) && flags[probeIndex] {
				score += term.weight
				matchedWeight += term.weight
				count++
				matchedTerms = append(matchedTerms, term.text)
				if !fieldSeen[term.field] {
					fieldSeen[term.field] = true
					matchedFields = append(matchedFields, term.field)
				}
			}
			probeIndex++
		}
		if count == 0 {
			continue
		}
		coverage := 0.0
		if totalWeight > 0 {
			coverage = matchedWeight / totalWeight
		}
		hits = append(hits, similarityHit{
			id: id, score: score, matchedTerms: matchedTerms,
			matchedTermCount: count, matchedFields: matchedFields,
			weightedCoverage: coverage,
		})
	}
	sortSimilarityHits(hits)
	if len(hits) > limit && limit > 0 {
		hits = hits[:limit]
	}
	return hits, nil
}

func sortSimilarityHits(hits []similarityHit) {
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && similarityHitLess(hits[j-1], hits[j]); j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
}

// similarityHitLess orders descending by score, then id for determinism.
func similarityHitLess(a, b similarityHit) bool {
	if a.score != b.score {
		return a.score < b.score
	}
	return a.id < b.id
}
