package engine

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func futureEntry(key string, reset time.Time) RankEntry {
	return RankEntry{Key: key, ResetAt: reset, HasFutureReset: true}
}

func TestRankCounts(t *testing.T) {
	// Spec section 21: 1 -> 100; 2 -> 200/100; ... 10 -> 1000..100.
	for _, n := range []int{1, 2, 3, 4, 5, 10} {
		t.Run(fmt.Sprintf("%d_accounts", n), func(t *testing.T) {
			entries := make([]RankEntry, 0, n)
			for i := 0; i < n; i++ {
				entries = append(entries, futureEntry(
					fmt.Sprintf("acct-%02d", i),
					baseTime.Add(time.Duration(i+1)*24*time.Hour),
				))
			}
			got := Rank(entries, 100, 100)
			for i := 0; i < n; i++ {
				want := 100 + 100*(n-1-i)
				key := fmt.Sprintf("acct-%02d", i)
				if got[key] != want {
					t.Errorf("rank %d (%s): priority = %d, want %d", i, key, got[key], want)
				}
			}
			// Earliest reset gets the highest priority; latest the floor.
			if got["acct-00"] != 100*n {
				t.Errorf("earliest priority = %d, want %d", got["acct-00"], 100*n)
			}
			if got[fmt.Sprintf("acct-%02d", n-1)] != 100 {
				t.Errorf("latest priority = %d, want 100", got[fmt.Sprintf("acct-%02d", n-1)])
			}
		})
	}
}

func TestRankDynamicFloorAndStep(t *testing.T) {
	entries := []RankEntry{
		futureEntry("a", baseTime.Add(1*time.Hour)),
		futureEntry("b", baseTime.Add(2*time.Hour)),
		futureEntry("c", baseTime.Add(3*time.Hour)),
	}
	got := Rank(entries, 50, 25)
	want := map[string]int{"a": 100, "b": 75, "c": 50}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rank(floor=50,step=25) = %v, want %v", got, want)
	}
}

func TestRankPriorityArithmeticExactAtNativeIntBoundary(t *testing.T) {
	entries := []RankEntry{
		futureEntry("a", baseTime.Add(1*time.Hour)),
		futureEntry("b", baseTime.Add(2*time.Hour)),
		futureEntry("c", baseTime.Add(3*time.Hour)),
	}
	got := Rank(entries, math.MaxInt-2, 1)
	want := map[string]int{"a": math.MaxInt, "b": math.MaxInt - 1, "c": math.MaxInt - 2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rank at MaxInt boundary = %v, want %v", got, want)
	}
}

func TestRankPriorityArithmeticSaturatesInsteadOfWrapping(t *testing.T) {
	entries := []RankEntry{
		futureEntry("a", baseTime.Add(1*time.Hour)),
		futureEntry("b", baseTime.Add(2*time.Hour)),
		futureEntry("c", baseTime.Add(3*time.Hour)),
		futureEntry("d", baseTime.Add(4*time.Hour)),
	}
	got := Rank(entries, math.MaxInt-1, math.MaxInt)
	want := map[string]int{
		"a": math.MaxInt,
		"b": math.MaxInt,
		"c": math.MaxInt,
		"d": math.MaxInt - 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rank overflow saturation = %v, want %v", got, want)
	}
	for key, priority := range got {
		if priority < 0 {
			t.Errorf("%s priority wrapped negative: %d", key, priority)
		}
	}
}

func TestRankUnknownSortsLastButCounts(t *testing.T) {
	// Spec section 11: unknown after confirmed future deadlines, still in N.
	entries := []RankEntry{
		{Key: "d-unknown"},
		futureEntry("a", baseTime.Add(1*time.Hour)),
		futureEntry("c", baseTime.Add(3*time.Hour)),
		futureEntry("b", baseTime.Add(2*time.Hour)),
	}
	got := Rank(entries, 100, 100)
	want := map[string]int{"a": 400, "b": 300, "c": 200, "d-unknown": 100}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rank = %v, want %v", got, want)
	}
}

func TestRankSingleUnknownAccount(t *testing.T) {
	got := Rank([]RankEntry{{Key: "only"}}, 100, 100)
	if got["only"] != 100 {
		t.Errorf("single unknown account priority = %d, want 100", got["only"])
	}
}

func TestRankMultipleUnknownDeterministicTieBreak(t *testing.T) {
	got := Rank([]RankEntry{{Key: "zeta"}, {Key: "alpha"}, {Key: "mid"}}, 100, 100)
	want := map[string]int{"alpha": 300, "mid": 200, "zeta": 100}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rank = %v, want %v", got, want)
	}
}

func TestRankExactTimestampTieIsDeterministic(t *testing.T) {
	// Spec section 12: identical instants must order deterministically and
	// must not flip between reconciliations; unique steps still assigned.
	at := baseTime.Add(48 * time.Hour)
	entries := []RankEntry{futureEntry("bbb", at), futureEntry("aaa", at)}
	first := Rank(entries, 100, 100)
	if first["aaa"] != 200 || first["bbb"] != 100 {
		t.Errorf("tie ranking = %v, want aaa=200 bbb=100", first)
	}
	for i := 0; i < 20; i++ {
		reordered := []RankEntry{futureEntry("aaa", at), futureEntry("bbb", at)}
		if got := Rank(reordered, 100, 100); !reflect.DeepEqual(got, first) {
			t.Fatalf("tie ranking flipped on iteration %d: %v vs %v", i, got, first)
		}
	}
}

func TestRankTimezoneOffsetsCompareAsInstants(t *testing.T) {
	// Spec section 21 precision: the same instant in different zones is a
	// tie; an earlier instant in a "later-looking" zone string still wins.
	zone := time.FixedZone("PDT", -7*60*60)
	instant := time.Date(2026, 9, 2, 3, 0, 0, 0, zone) // == 10:00Z
	sameInstantUTC := instant.UTC()
	earlierUTC := instant.Add(-time.Second).UTC()

	got := Rank([]RankEntry{
		futureEntry("tz-a", instant),
		futureEntry("tz-b", sameInstantUTC),
		futureEntry("early", earlierUTC),
	}, 100, 100)
	if got["early"] != 300 {
		t.Errorf("earlier instant priority = %d, want 300", got["early"])
	}
	// Equal instants fall back to the stable key.
	if got["tz-a"] != 200 || got["tz-b"] != 100 {
		t.Errorf("equal instants = %v, want tz-a=200 tz-b=100", got)
	}
}

func TestRankSecondLevelPrecision(t *testing.T) {
	// Spec section 8: timestamps differing by seconds (and less) must sort
	// correctly; no truncation to days or minutes.
	day := time.Date(2026, 9, 3, 13, 12, 41, 0, time.UTC)
	got := Rank([]RankEntry{
		futureEntry("later-second", day.Add(1*time.Second)),
		futureEntry("earlier-second", day),
		futureEntry("later-nano", day.Add(500*time.Millisecond)),
	}, 100, 100)
	want := map[string]int{"earlier-second": 300, "later-nano": 200, "later-second": 100}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rank = %v, want %v", got, want)
	}
}

func TestRankEmpty(t *testing.T) {
	if got := Rank(nil, 100, 100); len(got) != 0 {
		t.Errorf("Rank(nil) = %v, want empty", got)
	}
}
