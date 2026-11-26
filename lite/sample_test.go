package main

import (
	"math/rand"
	"testing"
)

func TestOfferReservoirFillsFirst(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var seen int64
	pool := []int64{}
	// 前 capacity 个必入池。
	for i := int64(1); i <= 5; i++ {
		seen, pool = OfferReservoir(seen, i, 5, pool, rng)
	}
	if len(pool) != 5 || seen != 5 {
		t.Fatalf("前 5 个应全入池: seen=%d pool=%+v", seen, pool)
	}
	for i, k := range pool {
		if k != int64(i+1) {
			t.Fatalf("前 5 个应保持原值: %+v", pool)
		}
	}
}

func TestOfferReservoirUniformity(t *testing.T) {
	// 从 1000 个键中抽 50 个，跑多轮，验证每个键入选频率大致均匀。
	const total = 1000
	const cap = 50
	// 2000 次试验保证统计稳定；蓄水池频率服从二项分布，单次波动可达 ±30%。
	const trials = 2000
	counts := make(map[int64]int)
	for seed := int64(0); seed < trials; seed++ {
		rng := rand.New(rand.NewSource(seed))
		seen := int64(0)
		pool := []int64{}
		for i := int64(1); i <= total; i++ {
			seen, pool = OfferReservoir(seen, i, cap, pool, rng)
		}
		for _, k := range pool {
			counts[k]++
		}
	}
	if len(counts) != total {
		t.Fatalf("所有键都应有机会入选，实际覆盖 %d/%d", len(counts), total)
	}
	// 期望频率 = trials * cap / total = 200*50/1000 = 10，允许 ±40% 波动。
	expected := float64(trials*cap) / total
	for k, c := range counts {
		diff := float64(c) - expected
		if diff < 0 {
			diff = -diff
		}
		if diff > expected*0.4 {
			t.Fatalf("键 %d 入选频率 %d 偏离期望 %.1f 过多，抽样可能有偏", k, c, expected)
		}
	}
}

func TestOfferReservoirWithHoles(t *testing.T) {
	// 主键有大量空洞：键集中在几个小区间，蓄水池仍应能抽到各处的键。
	rng := rand.New(rand.NewSource(42))
	keys := []int64{1, 2, 3, 1000000, 1000001, 999999999}
	seen := int64(0)
	pool := []int64{}
	for _, k := range keys {
		seen, pool = OfferReservoir(seen, k, 6, pool, rng)
	}
	// 池容量 6 >= 总数 6，应全入池，不因空洞漏抽。
	if len(pool) != 6 {
		t.Fatalf("空洞场景池应满: %+v", pool)
	}
}

func TestShuffleAndSplit(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	pool := []int64{1, 2, 3, 4, 5, 6, 7}
	rounds := ShuffleAndSplit(pool, 3, 3, rng)
	if len(rounds) != 3 {
		t.Fatalf("应切成 3 轮: %+v", rounds)
	}
	if len(rounds[0]) != 3 || len(rounds[1]) != 3 || len(rounds[2]) != 1 {
		t.Fatalf("各轮条数应为 3/3/1: %+v", rounds)
	}
	// 轮间不重叠：收集所有键应无重复。
	seen := make(map[int64]bool)
	for _, r := range rounds {
		for _, k := range r {
			if seen[k] {
				t.Fatalf("轮间出现重复键 %d", k)
			}
			seen[k] = true
		}
	}
	if len(seen) != 7 {
		t.Fatalf("应覆盖全部 7 个键，实际 %d", len(seen))
	}
}

func TestShuffleAndSplitShortPool(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	// 池子只有 2 个，要切 3 轮，后两轮应为空。
	rounds := ShuffleAndSplit([]int64{10, 20}, 3, 2, rng)
	if len(rounds) != 3 || len(rounds[0]) != 2 || len(rounds[1]) != 0 || len(rounds[2]) != 0 {
		t.Fatalf("短池切分不对: %+v", rounds)
	}
}
