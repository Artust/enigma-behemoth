package boss

// Reward tiers, keyed off the player's damage contribution percentage.
const (
	TierLegendary = "legendary"
	TierEpic      = "epic"
	TierRare      = "rare"
	TierUncommon  = "uncommon"
	TierCommon    = "common"
)

// TierFor maps a contribution percentage (0-100) to a reward tier. Thresholds
// are inclusive lower bounds.
func TierFor(pct float64) string {
	switch {
	case pct >= 20:
		return TierLegendary
	case pct >= 10:
		return TierEpic
	case pct >= 5:
		return TierRare
	case pct >= 1:
		return TierUncommon
	default:
		return TierCommon
	}
}

// RewardFor returns the reward payload granted for a tier. It is stored in the
// claim row so granting the reward is a durable write.
func RewardFor(tier string) map[string]any {
	switch tier {
	case TierLegendary:
		return map[string]any{"gold": 10000, "items": []string{"Behemoth Crown", "Legendary Chest"}}
	case TierEpic:
		return map[string]any{"gold": 5000, "items": []string{"Epic Chest"}}
	case TierRare:
		return map[string]any{"gold": 2000, "items": []string{"Rare Chest"}}
	case TierUncommon:
		return map[string]any{"gold": 500, "items": []string{"Uncommon Chest"}}
	default:
		return map[string]any{"gold": 100, "items": []string{"Common Chest"}}
	}
}
