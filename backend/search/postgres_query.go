// File overview: The read path of the PostgreSQL search backend. parseQuery
// stays the one grammar for both backends; this file compiles its result into
// the store's neutral query spec — filters onto the joined messages row, free
// text into a tsquery, the ranking knobs into the weight array, sender-boost
// cases and recency buckets. Field/term reporting comes back as weight-class
// matches (subject/addresses/body/attachments), which is coarser than Bleve's
// per-term locations but carries the same highlighting decisions.
//
// Fuzzy matching rides pg_trgm strict word similarity over the indexed word
// list and is available only where the extension is (EnsureTrigramSearch);
// without it the same queries run exact. Reductions against the Bleve path, recorded in
// docs/search-postgres-plan.md §5: query-side compound splitting relies on the
// split terms already in the vector, and Explain reports weight-class matches
// instead of a scorer tree.

package search

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"rolltop/backend/plugins"
	"rolltop/backend/store"
)

// Fuzzy tuning. pg_trgm word similarity of the classic transposition typo
// ("rehcnung" against a text containing "rechnung") measures 0.38, so the
// floors sit below that; anything at 0.22 and lower measured as unrelated.
// Short terms stay exact — two or three letters share trigrams with half the
// vocabulary.
// pgMaxQueryTerms is the working limit on how many words of one query reach
// SQL, below the store's own refusal. Each term costs a condition and a
// per-candidate to_tsquery evaluation, and a query past this width is a paste
// accident rather than a search; the extra words are dropped instead of
// failing the request.
const pgMaxQueryTerms = 32

const (
	pgFuzzyThresholdBalanced  = 0.35
	pgFuzzyThresholdForgiving = 0.30
	pgFuzzyMinRunesBalanced   = 5
	pgFuzzyMinRunesForgiving  = 4
)

// pgSenderBoostCeiling is the top of the scale the caller's sender boosts are
// expressed on (store.senderReadBoost caps at 8), and so the divisor that turns
// one into the fraction of a doubling the store's ranking multiplies by.
//
// Both boost lists are divided by it, not each by its own maximum: a contact
// weighs 1 against a sender read every time weighing 8, and that ratio is the
// statement the two lists together make. Normalizing them apart would silently
// promote every address in the contact book to the weight of the most-read
// correspondent.
const pgSenderBoostCeiling = 8

// pgRecencyBoostCeiling is the same divisor for the recency buckets: the largest
// boost any bias produces, so the strongest setting reaches one doubling on the
// freshest mail and the others land under it in proportion.
//
// One ceiling across every bias, not each bias divided by its own peak. Its own
// peak would map the top bucket of every setting to exactly 1.0, giving "light"
// and "strong" the same curve and leaving the recency-bias setting looking like
// it still worked while doing nothing. Read from the tables rather than written
// down, so a change to a bucket cannot leave a constant behind.
var pgRecencyBoostCeiling = func() float64 {
	peak := 0.0
	for _, bias := range []string{"none", "light", "normal", "strong"} {
		for _, bucket := range RecencyRankBuckets(bias) {
			peak = max(peak, bucket.Boost)
		}
	}
	return peak
}()

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
func pgSearchSpec(userID int64, parsed parsedQuery, opts SearchOptions, limit, offset int, fuzzyAvailable bool) store.MessageSearchQuery {
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
	if fuzzyAvailable && !parsed.TextQuoted && behavior.Fuzzy != "off" && spec.TSQuery != "" {
		minRunes := pgFuzzyMinRunesBalanced
		spec.FuzzyThreshold = pgFuzzyThresholdBalanced
		if behavior.Fuzzy == "forgiving" {
			minRunes = pgFuzzyMinRunesForgiving
			spec.FuzzyThreshold = pgFuzzyThresholdForgiving
		}
		terms := strings.Fields(normalizeSearchText(parsed.Text))
		if len(terms) > pgMaxQueryTerms {
			terms = terms[:pgMaxQueryTerms]
		}
		anyFuzzy := false
		for i, term := range terms {
			entry := store.MessageSearchTextTerm{TSQuery: pgTSQuery(term, false, i == len(terms)-1)}
			if len([]rune(term)) >= minRunes {
				entry.FuzzyTerm = term
				anyFuzzy = true
			}
			spec.TextTerms = append(spec.TextTerms, entry)
		}
		// All terms too short to fuzz: keep the single-tsquery plan, which the
		// planner serves straight from the GIN index.
		if !anyFuzzy {
			spec.TextTerms = nil
			spec.FuzzyThreshold = 0
		}
	}
	for _, negated := range parsed.NegatedText {
		if len(spec.NotTSQueries) >= pgMaxQueryTerms {
			break
		}
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
				Pattern: strings.ToLower(sender), Boost: boost.Boost * scale / pgSenderBoostCeiling,
			})
		}
	}
	appendBoosts(opts.SenderBoosts, behavior.SenderBoostScale)
	appendBoosts(opts.ContactBoosts, behavior.ContactBoostScale)
	// The Bleve path only nudges by recency when no explicit date range says
	// the user is already navigating time; mirrored here.
	if parsed.After.IsZero() && parsed.Before.IsZero() {
		for _, bucket := range RecencyRankBuckets(behavior.RecencyBias) {
			boost := bucket.Boost
			if pgRecencyBoostCeiling > 0 {
				boost /= pgRecencyBoostCeiling
			}
			spec.RecencyBuckets = append(spec.RecencyBuckets, store.MessageSearchRecencyBucket{
				MaxAgeSeconds: int64(bucket.Age / time.Second), Boost: boost,
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
//
// The class columns answer for the lexeme query only, so a row that came in
// through fuzzy similarity alone matches none of them. Reporting nothing there
// would quietly switch off attachment snippets, which read these names
// (api_message.go). The word list the similarity ran against is built from all
// four streams and cannot say which one held the similar word, so a fuzzy-only
// hit reports the full set: coarser than the Bleve locations, and honest about
// being unable to narrow it.
func pgMatchedFields(hit store.MessageSearchHit, fuzzy bool) []string {
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
	if len(fields) == 0 && fuzzy {
		return []string{"subject", "from", "body", "attachments"}
	}
	return fields
}

// SenderRankNudge and RecencyRankNudge report one boost as the backend in force
// actually applies it, so a page explaining a rank shows the number that acted.
//
// On Bleve a boost is added to a tf-idf score of comparable size and is its own
// explanation. On Postgres it is normalized and multiplied (see the ranking
// comment in store.SearchMessageIDs), where the caller's raw 8 is a doubling
// rather than eight points of a score whose whole range is under one.
func (s *Service) SenderRankNudge(boost float64) float64 {
	if s == nil || !s.PostgresBackend() {
		return boost
	}
	// The SQL caps the sum of the boosts naming one address; a panel showing
	// them one at a time caps each, which is the same ceiling for the single
	// boost it is describing.
	return min(boost/pgSenderBoostCeiling, 1)
}

func (s *Service) RecencyRankNudge(boost float64) float64 {
	if s == nil || !s.PostgresBackend() || pgRecencyBoostCeiling <= 0 {
		return boost
	}
	return boost / pgRecencyBoostCeiling
}

// specUsesFuzzy reports whether any term in the compiled spec can match by
// similarity, which is what makes an otherwise field-less hit explainable.
func specUsesFuzzy(spec store.MessageSearchQuery) bool {
	for _, term := range spec.TextTerms {
		if term.FuzzyTerm != "" {
			return true
		}
	}
	return false
}

func pgNeedleTerms(parsed parsedQuery) []string {
	needles := queryNeedles(parsed)
	terms := make([]string, 0, len(needles))
	for _, needle := range needles {
		terms = append(terms, needle.Term)
	}
	return terms
}

// pgFuzzyFallbackBelow is how few exact matches a query needs before typo
// tolerance is worth what it costs.
//
// Fuzzy matching probes a trigram similarity against the message's whole word
// list, which is a second copy of its text and, on any real message, stored out
// of line. Running it beside the lexeme query means reading that copy for every
// candidate the query touches - measured at seventeen times the cost of the
// exact search on an 85,000-message mailbox - and it earns nothing when the word
// was spelled correctly and the page is already full of real matches.
//
// So it becomes what it always was for the reader: a fallback. A query that
// already finds mail is answered exactly; one that finds almost nothing is the
// query that was probably mistyped, and that one pays. The gate is one bounded
// count off the index, and it reads a property of the query rather than of the
// page, so every page of the same search decides alike and paging stays
// consistent.
//
// A handful, not a page. Terms are ANDed, so a term the reader misspelled takes
// the whole query to zero exact matches - typo tolerance is needed at the bottom
// of that range, not near a full page of hits. Set at fifty this fired for
// nearly every specific search anyone types, which is exactly the search that
// least needs it: six real answers became a page of near-spellings ranked
// alongside them. The cushion above zero is for the misspelling that happens to
// be a word in somebody's mail.
const pgFuzzyFallbackBelow = 5

// pgExactSpec is one spec with its fuzzy half removed: membership and score
// both fall back to the lexeme query alone.
func pgExactSpec(spec store.MessageSearchQuery) store.MessageSearchQuery {
	spec.TextTerms = nil
	spec.FuzzyThreshold = 0
	return spec
}

// pgResolveFuzzy drops the fuzzy half of a spec when the exact query alone
// finds enough mail to fill a page.
func (s *Service) pgResolveFuzzy(ctx context.Context, spec store.MessageSearchQuery) (store.MessageSearchQuery, error) {
	if !specUsesFuzzy(spec) {
		return spec, nil
	}
	exact := pgExactSpec(spec)
	found, err := s.pg.CountMessageSearchMatches(ctx, exact, pgFuzzyFallbackBelow)
	if err != nil {
		return spec, fmt.Errorf("postgres search gate: %w", err)
	}
	if found >= pgFuzzyFallbackBelow {
		return exact, nil
	}
	return spec, nil
}

// pgSearchOrder maps the package's result order onto the store's spelling of it.
func pgSearchOrder(order SearchOrder) store.MessageSearchOrder {
	switch order {
	case SearchOrderNewest:
		return store.MessageSearchOrderNewest
	case SearchOrderOldest:
		return store.MessageSearchOrderOldest
	default:
		return store.MessageSearchOrderRelevance
	}
}

func (s *Service) pgSearchHits(ctx context.Context, userID int64, queryText string, limit, offset int, opts SearchOptions) ([]Hit, error) {
	if limit <= 0 || limit > maxHitsPerRequest {
		limit = 50
	}
	offset = max(offset, 0)
	parsed := parseQuery(queryText)
	spec := pgSearchSpec(userID, parsed, opts, limit, offset, s.pg.TrigramSearchEnabled())
	// Only the results list orders by date. The spec builder is shared with
	// match and explain, which ask the ranking question, so the order is set
	// here rather than in there.
	spec.Order = pgSearchOrder(opts.Order)
	spec, err := s.pgResolveFuzzy(ctx, spec)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.SearchMessageIDs(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("postgres search: %w", err)
	}
	terms := pgNeedleTerms(parsed)
	fuzzy := specUsesFuzzy(spec)
	hits := make([]Hit, 0, len(rows))
	for _, row := range rows {
		hits = append(hits, Hit{
			ID:         row.MessageID,
			Score:      row.Score,
			Terms:      terms,
			Fields:     pgMatchedFields(row, fuzzy),
			QueryTerms: terms,
		})
	}
	return hits, nil
}

// pgExplainIDChunk bounds one restriction list, matching the store's own cap.
// A thread can hold more messages than that, and the Bleve path had no cap, so
// the ids are walked in chunks and the best-scoring row across them wins -
// which is the single hit the Bleve request asked for.
const pgExplainIDChunk = 500

func (s *Service) pgExplainMessageIDs(ctx context.Context, userID int64, messageIDs []int64, queryText string, opts SearchOptions) (ExplanationResult, bool, error) {
	parsed := parseQuery(queryText)
	spec := pgSearchSpec(userID, parsed, opts, 1, 0, s.pg.TrigramSearchEnabled())
	spec, err := s.pgResolveFuzzy(ctx, spec)
	if err != nil {
		return ExplanationResult{}, false, err
	}
	ids := make([]int64, 0, len(messageIDs))
	seen := make(map[int64]bool, len(messageIDs))
	for _, id := range messageIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return ExplanationResult{}, false, nil
	}
	var best store.MessageSearchHit
	found := false
	for start := 0; start < len(ids); start += pgExplainIDChunk {
		if err := ctx.Err(); err != nil {
			return ExplanationResult{}, false, err
		}
		chunk := spec
		chunk.MessageIDs = ids[start:min(start+pgExplainIDChunk, len(ids))]
		rows, err := s.pg.SearchMessageIDs(ctx, chunk)
		if err != nil {
			return ExplanationResult{}, false, fmt.Errorf("postgres match: %w", err)
		}
		if len(rows) > 0 && (!found || rows[0].Score > best.Score) {
			best = rows[0]
			found = true
		}
	}
	if !found {
		return ExplanationResult{}, false, nil
	}
	row := best
	terms := pgNeedleTerms(parsed)
	fields := pgMatchedFields(row, specUsesFuzzy(spec))
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

// pgSimilarityTSQuery renders one similarity term. Bleve's match query ORs a
// term's words, and only the multi-word domain term is switched to AND
// (similarity.go), so the same split is made here: anything else would make
// the Postgres backend stricter than the Bleve one for the same request.
func pgSimilarityTSQuery(term normalizedSimilarityTerm) string {
	words := strings.Fields(normalizeSearchText(term.text))
	if len(words) == 0 {
		return ""
	}
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = "'" + strings.ReplaceAll(word, "'", "''") + "'"
	}
	if term.field == plugins.SimilarityFieldFromDomain && len(quoted) > 1 {
		return strings.Join(quoted, " & ")
	}
	return strings.Join(quoted, " | ")
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
	probeTerms := make([]normalizedSimilarityTerm, 0, len(terms))
	for _, term := range terms {
		tsq := pgSimilarityTSQuery(term)
		if tsq == "" {
			continue
		}
		probes = append(probes, store.MessageSearchTermProbe{TSQuery: tsq, WeightClass: pgSimilarityClass(term.field)})
		probeTerms = append(probeTerms, term)
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
		for probeIndex, term := range probeTerms {
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

// sortSimilarityHits orders descending by score, then by id so a tie is
// resolved the same way on every run.
func sortSimilarityHits(hits []similarityHit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].id > hits[j].id
	})
}
