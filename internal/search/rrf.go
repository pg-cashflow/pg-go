package search

const rrfK = 60

// FuseRRF merges ranked lists with reciprocal rank fusion (k=60).
func FuseRRF(lists [][]Result) []Result {
	scores := make(map[string]float64)
	byKey := make(map[string]Result)
	for _, list := range lists {
		for i, r := range list {
			key := string(r.Type) + ":" + r.ID
			scores[key] += 1.0 / (float64(rrfK) + float64(i+1))
			if _, ok := byKey[key]; !ok {
				byKey[key] = r
			}
		}
	}
	out := make([]Result, 0, len(byKey))
	for key, r := range byKey {
		r.Score = scores[key]
		out = append(out, r)
	}
	sortResultsDesc(out)
	return out
}

func sortResultsDesc(rs []Result) {
	for i := 0; i < len(rs); i++ {
		for j := i + 1; j < len(rs); j++ {
			if rs[j].Score > rs[i].Score {
				rs[i], rs[j] = rs[j], rs[i]
			}
		}
	}
}
