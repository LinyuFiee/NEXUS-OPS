package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// Search is read-only. scopes must be derived from the authenticated session;
// empty scopes fail closed. Only active, unexpired entries in those exact scopes
// participate in candidate selection AND BM25 corpus/term statistics. It reads
// matching disk postings rather than loading all memory texts into Go.
//
// limit is capped at 10; maxChars at 6000. Non-positive budgets return no hits.
// The character budget covers the complete JSON hit array (Unicode code points,
// including metadata/escaping), as used by the engine, not merely item.Text.
// Oversized hits are skipped rather than truncated into misleading assertions.
// Very small budgets may therefore yield no hits. Scores express relevance,
// never correctness; searches do not mutate importance, history or usage counts.
func (s *Store) Search(ctx context.Context, query string, scopes []string, limit, maxChars int) ([]Hit, error) {
	out := []Hit{}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(scopes) == 0 || limit <= 0 || maxChars <= 0 {
		return out, nil
	}
	if len(scopes) > 128 || !utf8.ValidString(query) || utf8.RuneCountInString(query) > MaxQueryRunes {
		return nil, fmt.Errorf("%w: 检索范围或查询长度超限", ErrInvalid)
	}
	limit, maxChars = min(limit, MaxResults), min(maxChars, MaxSearchChars)
	uniqueScopes := []string{}
	seenScopes := map[string]bool{}
	for _, scope := range scopes {
		if !validScope(scope) {
			return nil, fmt.Errorf("%w: 检索 scope 必须明确有效", ErrInvalid)
		}
		if !seenScopes[scope] {
			uniqueScopes = append(uniqueScopes, scope)
			seenScopes[scope] = true
		}
	}
	queryTerms := []string{}
	seenTerms := map[string]bool{}
	for _, term := range tokens(query) {
		if !seenTerms[term] {
			queryTerms = append(queryTerms, term)
			seenTerms[term] = true
			if len(queryTerms) == 64 {
				break // Bound SQL variables and postings processed per search.
			}
		}
	}
	if len(queryTerms) == 0 || maxChars < 2 {
		return out, nil
	}
	args := make([]any, 0, len(uniqueScopes)+len(queryTerms)+1)
	for _, scope := range uniqueScopes {
		args = append(args, scope)
	}
	args = append(args, s.now().UTC().Format(timeLayout))
	for _, term := range queryTerms {
		args = append(args, term)
	}
	// MATERIALIZED explicitly builds the scope/status/expiry-filtered relation
	// before ranking. Window df and corpus statistics see that relation only;
	// a prolific owner/other group cannot crowd out or alter a group's results.
	statement := `WITH eligible AS MATERIALIZED (
 SELECT id,token_count,importance,updated_at FROM mem_items
 WHERE scope IN (` + placeholders(len(uniqueScopes)) + `) AND status='active'
 AND (expires_at IS NULL OR expires_at>?)
), stats AS (
 SELECT COUNT(*) AS n,COALESCE(AVG(token_count),0) AS avg_length FROM eligible
), matches AS (
 SELECT t.item_id,t.term,t.tf,e.token_count,e.importance,e.updated_at,
 COUNT(*) OVER (PARTITION BY t.term) AS df
 FROM mem_terms t JOIN eligible e ON e.id=t.item_id
 WHERE t.term IN (` + placeholders(len(queryTerms)) + `)
)
SELECT m.item_id,m.term,m.tf,m.token_count,m.importance,m.updated_at,m.df,s.n,s.avg_length
FROM matches m CROSS JOIN stats s`
	// One read snapshot keeps postings, lifecycle flags, statistics and selected
	// item bodies consistent even if another Store changes a record meanwhile.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id         string
		score      float64
		importance float64
		updated    string
		terms      []string
	}
	candidates := map[string]*candidate{}
	for rows.Next() {
		var id, term, updated string
		var tf, length, df, total int
		var importance, average float64
		if err = rows.Scan(&id, &term, &tf, &length, &importance, &updated, &df, &total, &average); err != nil {
			rows.Close()
			return nil, err
		}
		if average <= 0 || total == 0 {
			continue
		}
		entry := candidates[id]
		if entry == nil {
			entry = &candidate{id: id, importance: importance, updated: updated}
			candidates[id] = entry
		}
		const k1, b = 1.2, 0.75
		idf := math.Log1p((float64(total-df) + 0.5) / (float64(df) + 0.5))
		entry.score += idf * float64(tf) * (k1 + 1) / (float64(tf) + k1*(1-b+b*float64(length)/average))
		entry.terms = append(entry.terms, term)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	ranked := make([]*candidate, 0, len(candidates))
	for _, entry := range candidates {
		ranked = append(ranked, entry)
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.importance != b.importance {
			return a.importance > b.importance // Explicit metadata, tie-break only.
		}
		if a.updated != b.updated {
			return a.updated > b.updated
		}
		return a.id < b.id
	})
	// Fetch at most 100 top-ranked bodies, not the whole corpus, even when the
	// caller gives a budget too small to hold any result. No partial-text hits.
	used := 2 // JSON array brackets.
	for _, entry := range ranked[:min(len(ranked), 100)] {
		if len(out) == limit {
			break
		}
		item, err := scanItem(tx.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM mem_items WHERE id=?", entry.id))
		if err != nil {
			return nil, err
		}
		sort.Strings(entry.terms)
		shown := entry.terms[:min(len(entry.terms), 6)]
		hit := Hit{Item: item, Score: entry.score, Why: []string{
			"文本相关度 BM25，不代表正确性",
			"命中词：" + strings.Join(shown, "、"),
		}}
		body, err := json.Marshal(hit)
		if err != nil {
			return nil, err
		}
		cost := utf8.RuneCount(body)
		if len(out) != 0 {
			cost++ // Comma between hits.
		}
		if used+cost > maxChars {
			continue
		}
		used += cost
		out = append(out, hit)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
