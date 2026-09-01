package engine

import (
	"math"
	"sort"
	"time"
)

// RankEntry is one rankable account for deterministic priority assignment.
type RankEntry struct {
	// Key is the stable deterministic tie-break identifier.
	Key string
	// ResetAt is the exact weekly reset deadline; only meaningful when
	// HasFutureReset is true.
	ResetAt time.Time
	// HasFutureReset reports whether ResetAt is a confirmed/stale future
	// deadline usable for ordering.
	HasFutureReset bool
}

// Rank assigns priorities to one provider group's rankable accounts:
//
//	priority = floor + step * (N - 1 - rank)
//
// where rank is the zero-based position after sorting by:
//
//  1. confirmed future weekly reset ascending (earliest reset gets the
//     highest priority),
//  2. accounts without a usable future reset (awaiting_new_window/unknown)
//     after all future resets,
//  3. stable key ascending as the deterministic tie-break.
//
// Timestamps are compared as exact instants: equal instants expressed in
// different timezone offsets compare equal, and any sub-second difference
// orders the accounts. Every account, including unknown/awaiting ones,
// participates in the count N so the managed pool stays predictable. If the
// formula exceeds the native int range, priorities saturate at the nearest int
// boundary instead of wrapping.
func Rank(entries []RankEntry, floor, step int) map[string]int {
	ordered := make([]RankEntry, len(entries))
	copy(ordered, entries)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.HasFutureReset != b.HasFutureReset {
			return a.HasFutureReset
		}
		if a.HasFutureReset && b.HasFutureReset && !a.ResetAt.Equal(b.ResetAt) {
			return a.ResetAt.Before(b.ResetAt)
		}
		return a.Key < b.Key
	})
	// Build priorities from the floor upward so no account-count
	// multiplication can overflow. Once the native int ceiling is reached,
	// later additions saturate there rather than wrapping into negative values.
	priorities := make([]int, len(ordered))
	priority := floor
	for i := len(ordered) - 1; i >= 0; i-- {
		priorities[i] = priority
		priority = saturatingAdd(priority, step)
	}

	out := make(map[string]int, len(ordered))
	for i, entry := range ordered {
		out[entry.Key] = priorities[i]
	}
	return out
}

func saturatingAdd(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	if b < 0 && a < math.MinInt-b {
		return math.MinInt
	}
	return a + b
}
