package boss

import "testing"

// TestRewardIsMonotonicByTier: gold strictly decreases from legendary to common,
// so a better contribution always yields a better reward (ordering, not values).
func TestRewardIsMonotonicByTier(t *testing.T) {
	order := []string{TierLegendary, TierEpic, TierRare, TierUncommon, TierCommon}
	prev := int(^uint(0) >> 1) // max int
	for _, tier := range order {
		gold, ok := RewardFor(tier)["gold"].(int)
		if !ok {
			t.Fatalf("reward for %q has non-int gold: %v", tier, RewardFor(tier)["gold"])
		}
		if gold <= 0 {
			t.Errorf("reward for %q has non-positive gold %d", tier, gold)
		}
		if gold >= prev {
			t.Errorf("gold for %q = %d is not strictly less than the higher tier (%d); reward must increase with tier", tier, gold, prev)
		}
		prev = gold
	}
}

// TestTierForIsMonotonic: tier rank never decreases as contribution grows.
func TestTierForIsMonotonic(t *testing.T) {
	rank := map[string]int{
		TierCommon: 0, TierUncommon: 1, TierRare: 2, TierEpic: 3, TierLegendary: 4,
	}
	prev := -1
	for pct := 0.0; pct <= 100.0; pct += 0.1 {
		r, ok := rank[TierFor(pct)]
		if !ok {
			t.Fatalf("TierFor(%v) returned unknown tier %q", pct, TierFor(pct))
		}
		if r < prev {
			t.Fatalf("TierFor not monotonic: pct=%.1f gave rank %d after a higher rank %d", pct, r, prev)
		}
		prev = r
	}
}

func TestTierFor(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{100, TierLegendary},
		{20, TierLegendary},
		{19.999, TierEpic},
		{10, TierEpic},
		{9.5, TierRare},
		{5, TierRare},
		{4.9, TierUncommon},
		{1, TierUncommon},
		{0.999, TierCommon},
		{0, TierCommon},
	}
	for _, c := range cases {
		if got := TierFor(c.pct); got != c.want {
			t.Errorf("TierFor(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}

// TestTierForOutOfRange guards the domain ends: <1% (incl. 0 and negative) is
// Common, >=100 (incl. rounding past a full clear) stays Legendary.
func TestTierForOutOfRange(t *testing.T) {
	cases := []struct {
		pct  float64
		want string
	}{
		{-0.0001, TierCommon},
		{-100, TierCommon},
		{0.5, TierCommon},
		{100.0001, TierLegendary},
		{1000, TierLegendary},
	}
	for _, c := range cases {
		if got := TierFor(c.pct); got != c.want {
			t.Errorf("TierFor(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
}

func TestRewardForKnownTiers(t *testing.T) {
	for _, tier := range []string{TierLegendary, TierEpic, TierRare, TierUncommon, TierCommon} {
		r := RewardFor(tier)
		if _, ok := r["gold"]; !ok {
			t.Errorf("reward for %q missing gold", tier)
		}
		if _, ok := r["items"]; !ok {
			t.Errorf("reward for %q missing items", tier)
		}
	}
}

func TestRewardForUnknownDefaultsToCommon(t *testing.T) {
	got := RewardFor("nonsense")
	want := RewardFor(TierCommon)
	if got["gold"] != want["gold"] {
		t.Errorf("unknown tier should default to common reward")
	}
}
