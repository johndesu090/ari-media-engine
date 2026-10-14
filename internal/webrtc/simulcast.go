package webrtc

import "strings"

// selectRID returns the preferred (highest-quality) RID from rids. Known
// simulcast names (h/m/l, high/medium/low) rank above arbitrary RIDs; ties are
// broken lexicographically so the result is deterministic.
func selectRID(rids []string) string {
	best := ""
	bestRank := -1
	for _, rid := range rids {
		r := ridRank(rid)
		if r > bestRank || (r == bestRank && rid > best) {
			best, bestRank = rid, r
		}
	}
	return best
}

func ridRank(rid string) int {
	switch strings.ToLower(rid) {
	case "h", "high", "hi":
		return 3
	case "m", "medium", "mid":
		return 2
	case "l", "low":
		return 1
	default:
		return 0
	}
}
