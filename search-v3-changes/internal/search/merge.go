package search

import "sort"

// Merge tuning. Each type's hits arrive already ranked by the repository
// (exact > prefix > word-start > contains > fuzzy > recency). Merge only
// decides how the per-type lists are interleaved into one global list.
const (
	scoreIntentBase = 100.0 // types that match the query's intent
	scoreOtherBase  = 50.0  // everything else
	scoreRankStep   = 10.0  // penalty per position within a type
)

// intentTypes maps the classified query tokens to the entity types a user most
// likely means. A UTR / due code should surface financial records first; a
// name should surface people first. Types outside the intent are still
// returned, just ranked lower.
func intentTypes(tokens []Token) map[EntityType]bool {
	out := map[EntityType]bool{}
	for _, t := range tokens {
		switch t.Kind {
		case TokenCode:
			for _, et := range []EntityType{TypeDue, TypePayment, TypePaymentReport, TypeBankTransaction, TypeSettlement, TypeRefund, TypePayout} {
				out[et] = true
			}
		case TokenPhone:
			for _, et := range []EntityType{TypeTenant, TypeJoinRequest, TypePayout} {
				out[et] = true
			}
		case TokenShortNumber:
			for _, et := range []EntityType{TypeTenant, TypeDue} {
				out[et] = true
			}
		default: // TokenText
			for _, et := range []EntityType{TypeTenant, TypeJoinRequest, TypePayout, TypeInspection, TypeHazard, TypeViolation} {
				out[et] = true
			}
		}
	}
	return out
}

// MergeResults builds the final global list from per-type ranked hits.
//
//  1. Score = type base (intent-aware) minus a penalty per in-type position.
//  2. The best hit of every type that matched is guaranteed a slot (when limit
//     allows), so no entity type is starved by the global limit.
//  3. Remaining slots go to the highest scores; output is score-descending.
//
// Input order within a type is preserved. The function is deterministic.
func MergeResults(in []Result, tokens []Token, limit int) []Result {
	if limit <= 0 || len(in) == 0 {
		return nil
	}
	intent := intentTypes(tokens)

	type scored struct {
		r     Result
		rank  int // position within its type
		order int // original index, for stable ties
	}
	seen := map[EntityType]int{}
	all := make([]scored, 0, len(in))
	for i, r := range in {
		rank := seen[r.Type]
		seen[r.Type]++
		base := scoreOtherBase
		if intent[r.Type] {
			base = scoreIntentBase
		}
		r.Score = base - float64(rank)*scoreRankStep
		all = append(all, scored{r: r, rank: rank, order: i})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].r.Score != all[j].r.Score {
			return all[i].r.Score > all[j].r.Score
		}
		return all[i].order < all[j].order
	})

	picked := make([]bool, len(all))
	n := 0
	// Pass 1: one guaranteed slot per type, best-scoring types first.
	for i, s := range all {
		if n >= limit {
			break
		}
		if s.rank == 0 {
			picked[i] = true
			n++
		}
	}
	// Pass 2: fill by score.
	for i := range all {
		if n >= limit {
			break
		}
		if !picked[i] {
			picked[i] = true
			n++
		}
	}
	out := make([]Result, 0, n)
	for i, s := range all {
		if picked[i] {
			out = append(out, s.r)
		}
	}
	return out
}
