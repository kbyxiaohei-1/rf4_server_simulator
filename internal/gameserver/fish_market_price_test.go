package gameserver

import (
	"math"
	"testing"

	"rf4server/internal/store"
)

// 水域倍率必须真正生效：不同 market_pct 的地图返回不同覆盖价。
func TestMarketOverlayAppliesMapMultiplier(t *testing.T) {
	low := marketSilverPerKgOverlay("level_001_lake", "bream", 1)            // 0.831
	mid := marketSilverPerKgOverlay("level_004_torfyanoe_ozero", "bream", 1) // 0.916
	base := marketSilverPerKgOverlay("level_009_ahtuba", "bream", 1)         // 1.0
	if low <= 0 || mid <= 0 || base <= 0 {
		t.Fatalf("覆盖价缺失: low=%v mid=%v base=%v", low, mid, base)
	}
	if !(low < mid && mid < base) {
		t.Fatalf("水域倍率未生效: low=%v mid=%v base=%v", low, mid, base)
	}
	if got, want := low/base, marketMapPct("level_001_lake"); math.Abs(got-want) > 0.01 {
		t.Fatalf("低倍率折算 = %.4f，期望 %.4f", got, want)
	}
}

// 客户端询价使用 <map>_fishmarket 场景键，必须归一到真实地图键。
func TestMarketOverlayNormalizesFishmarketKey(t *testing.T) {
	got := marketSilverPerKgOverlay("level_004_fishmarket", "bream", 1)
	want := marketSilverPerKgOverlay("level_004_torfyanoe_ozero", "bream", 1)
	if got != want {
		t.Fatalf("fishmarket 键未归一: got=%v want=%v", got, want)
	}
}

// 海图等地图使用 fishmarket_<编号> 的前缀写法，同样要命中真实地图键。
func TestMarketOverlayNormalizesFishmarketPrefixKey(t *testing.T) {
	got := marketSilverPerKgOverlay("fishmarket_012", "bream", 1)
	want := marketSilverPerKgOverlay("level_012_north_sea", "bream", 1)
	if got <= 0 || got != want {
		t.Fatalf("fishmarket_012 未归一: got=%v want=%v", got, want)
	}
}

// 每日系数：范围 0.95–1.05、同日稳定、跨日变化。
func TestDailyMarketFactorRangeAndStability(t *testing.T) {
	old := marketDayForced
	defer func() { marketDayForced = old }()

	marketDayForced = "20260101"
	first := dailyMarketFactor("bream", 0)
	if first < 0.95 || first > 1.05 {
		t.Fatalf("系数越界: %v", first)
	}
	if again := dailyMarketFactor("bream", 6*3600); again != first {
		t.Fatalf("同日不稳定: %v vs %v", first, again)
	}

	marketDayForced = "20260102"
	if next := dailyMarketFactor("bream", 0); next == first {
		t.Fatalf("跨日未变化: %v", next)
	}
}

// 同一鱼在同一时刻的询价与结算价必须一致。
func TestMarketPriceStableWithinSameInstant(t *testing.T) {
	e := &store.KeepnetEntry{Species: "bream", Cat: 2, WeightG: 3500, TS: 0}
	now := 1750000000.0
	first := marketPriceCentsForMap(e, now, "level_001_fishmarket")
	second := marketPriceCentsForMap(e, now, "level_001_fishmarket")
	if first != second {
		t.Fatalf("同一时刻报价不一致: %d vs %d", first, second)
	}
	if first <= 0 {
		t.Fatalf("报价必须为正: %d", first)
	}
}
