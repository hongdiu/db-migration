package main

import (
	"math"
	"math/rand"
	"testing"
)

// TestOfferReservoirUniformity 验证蓄水池抽样的每一步判定都是均匀的：
// 对 1..200 的序列取 20 个样本，每个数入选的概率应接近 20/200。
func TestOfferReservoirUniformity(t *testing.T) {
	const total = 200
	const want = 20
	const rounds = 8000
	rng := rand.New(rand.NewSource(20261117))
	hits := make(map[int64]int, total)
	for r := 0; r < rounds; r++ {
		pool := make([]int64, 0, want)
		for i := int64(1); i <= total; i++ {
			offerReservoir(&pool, i, i, want, rng)
		}
		if len(pool) != want {
			t.Fatalf("池容量应为 %d，实际 %d", want, len(pool))
		}
		for _, k := range pool {
			hits[k]++
		}
	}
	wantRate := float64(want) / total
	for i := int64(1); i <= total; i++ {
		rate := float64(hits[i]) / rounds
		if math.Abs(rate-wantRate) > wantRate*0.35 {
			t.Errorf("主键 %d 入选频率 %.4f 偏离期望 %.4f 过多", i, rate, wantRate)
		}
	}
	// 每个位置都应被覆盖过，说明没有系统性漏抽。
	if len(hits) != total {
		t.Errorf("覆盖不足: 仅覆盖 %d/%d 个主键", len(hits), total)
	}
}

// TestReservoirAvoidsHoleDegeneration 是 P2-1 的核心回归测试。
//
// 数据分布：主键 1..100 连续，之后是一个巨大空洞，空洞末尾一行主键 1000001。
// 旧算法（在 [min,max] 上随机取 target 再取 >= target 的第一条）会让 1000001 这一行
// 以约 99.99% 的概率独占总抽样，其余 100 行几乎抽不到。
// 蓄水池抽样必须做到：每次抽满 20 条互不相同的行，1000001 只按 20/101 的比例出现。
func TestReservoirAvoidsHoleDegeneration(t *testing.T) {
	keys := make([]int64, 0, 101)
	for i := int64(1); i <= 100; i++ {
		keys = append(keys, i)
	}
	keys = append(keys, 1000001)

	const want = 20
	const rounds = 4000
	rng := rand.New(rand.NewSource(99))
	sparseHit := 0
	for r := 0; r < rounds; r++ {
		pool := make([]int64, 0, want)
		for i, k := range keys {
			offerReservoir(&pool, k, int64(i+1), want, rng)
		}
		if len(pool) != want {
			t.Fatalf("每次应抽满 %d 条，实际 %d", want, len(pool))
		}
		seen := make(map[int64]struct{}, want)
		for _, k := range pool {
			if _, dup := seen[k]; dup {
				t.Fatalf("同一行被重复抽到: %d", k)
			}
			seen[k] = struct{}{}
			if k == 1000001 {
				sparseHit++
			}
		}
	}

	// 空洞末尾行只应占约 want/101，而不是近乎 100%。
	rate := float64(sparseHit) / rounds
	expected := float64(want) / float64(len(keys))
	if rate > expected*2.5 {
		t.Errorf("空洞末尾行仍被过度抽样: 出现率 %.3f，期望约 %.3f（旧算法约 1.0）", rate, expected)
	}
	if rate < expected*0.3 {
		t.Errorf("空洞末尾行几乎抽不到: 出现率 %.3f，期望约 %.3f", rate, expected)
	}
}

// TestOfferReservoirEdgeCases 覆盖边界：want<=0、seen<=0、序列长度小于池容量。
func TestOfferReservoirEdgeCases(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pool := []int64{}
	offerReservoir(&pool, 5, 1, 0, rng)
	if len(pool) != 0 {
		t.Errorf("want=0 时不应入池，实际 %v", pool)
	}
	offerReservoir(&pool, 5, 0, 10, rng)
	if len(pool) != 0 {
		t.Errorf("seen=0 时不应入池，实际 %v", pool)
	}
	for i := int64(1); i <= 3; i++ {
		offerReservoir(&pool, i, i, 10, rng)
	}
	if len(pool) != 3 {
		t.Errorf("序列短于池容量时应全部入池，实际 %v", pool)
	}
}

// TestSplitPool 校验多轮切分：不重叠、并集等于池、行数不足时的退化行为。
func TestSplitPool(t *testing.T) {
	pool := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	chunks := splitPool(pool, 3)
	if len(chunks) != 3 {
		t.Fatalf("应切成 3 份，实际 %d", len(chunks))
	}
	sizes := []int{len(chunks[0]), len(chunks[1]), len(chunks[2])}
	if sizes[0] != 4 || sizes[1] != 3 || sizes[2] != 3 {
		t.Errorf("切分大小应为 4/3/3，实际 %v", sizes)
	}
	seen := make(map[int64]bool, len(pool))
	total := 0
	for _, c := range chunks {
		for _, k := range c {
			if seen[k] {
				t.Errorf("主键 %d 在多轮中重复出现", k)
			}
			seen[k] = true
			total++
		}
	}
	if total != len(pool) {
		t.Errorf("切分后总数应为 %d，实际 %d", len(pool), total)
	}

	// 行数少于轮数：前几轮各 1 条，其余为空。
	chunks = splitPool([]int64{7, 8}, 5)
	if len(chunks) != 5 {
		t.Fatalf("应切成 5 份，实际 %d", len(chunks))
	}
	if len(chunks[0]) != 1 || len(chunks[1]) != 1 || len(chunks[2]) != 0 {
		t.Errorf("行数不足时前面几轮应各 1 条，其余为空，实际 %v", chunks)
	}

	// 空池与非法轮数。
	if got := splitPool(nil, 3); len(got) != 3 || len(got[0]) != 0 {
		t.Errorf("空池应返回等长空切片，实际 %v", got)
	}
	if got := splitPool([]int64{1}, 0); len(got) != 0 {
		t.Errorf("轮数为 0 应返回空，实际 %v", got)
	}
}
