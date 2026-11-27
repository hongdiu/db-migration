// 抽样：从归档表取少量主键，用于逐字段比对。
//
// 主路径是"分区随机探针"：把主键范围切成若干等宽区间（数量是样本数的 3 倍，用于对抗主键空洞），
// 每个区间先由种子生成一个随机探针点，再在数据库里取离探针最近的一条候选（先向后找、找不到再向前找），
// 拿到候选后洗牌取所需数量。探针由种子决定，换种子就换一批候选，
// 反复执行 sample 才能真正扩大抽查面（早先每区间取最小主键时，候选集是固定的，换种子只在同一批里换）。
// 全程不把全部主键读到程序里，几千万行的表也只返回几百行，避免把几千万个主键逐行传输到客户端的开销。
//
// 降级路径是"蓄水池抽样"：候选不足时（表很小或主键空洞极多）改回原来的流式全扫描。
// 蓄水池对每一行等概率，不会因主键空洞整段漏抽，适合小表。
//
// 两条路径最终都把选中的主键按轮切分，轮间不重叠。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
)

// MakeSampleRanges 把主键范围切成 count 个等宽区间，供分区取点使用。
// 主键有空洞时部分区间会查不到行，调用方跳过即可；区间宽度至少为 1，不会出现空区间。
// 跨度用无符号计算，避免两个极值相减溢出。
func MakeSampleRanges(minKey, maxKey int64, count int) []BlockRange {
	if count <= 0 || minKey > maxKey {
		return nil
	}
	minU, maxU := uint64(minKey), uint64(maxKey)
	span := maxU - minU
	// 按跨度等分，余数分摊到前面的区间，保证各区间尽量等宽且连续覆盖。
	base := span / uint64(count)
	extra := span % uint64(count)

	ranges := make([]BlockRange, 0, count)
	cur := minU
	for i := 0; i < count; i++ {
		if cur > maxU {
			break
		}
		width := base
		if uint64(i) < extra {
			width++ // 前 extra 个区间多覆盖一个键值
		}
		if width == 0 {
			break // 键值不够分，剩余区间留空
		}
		lo := int64(cur)
		next := cur + width
		hi := maxKey
		if next <= maxU && i < count-1 {
			hi = int64(next - 1)
		}
		ranges = append(ranges, BlockRange{Lo: lo, Hi: hi})
		if hi == maxKey {
			break
		}
		cur = next
	}
	// 兜底：最后一个区间必须覆盖到最大主键，避免尾部遗漏。
	if len(ranges) > 0 {
		ranges[len(ranges)-1].Hi = maxKey
	}
	return ranges
}

// ProbePoints 为每个区间生成一个随机探针点，供"取离探针最近的候选"使用。
// 探针在 [Lo,Hi] 内按跨度均匀分布，主键有空洞也不会越界。
// 同一 seed 得到完全相同的探针，因此 -seed 能复现整轮抽样；不同 seed 探针不同，候选随之改变。
func ProbePoints(ranges []BlockRange, rng *rand.Rand) []int64 {
	points := make([]int64, len(ranges))
	for i, r := range ranges {
		points[i] = randomPointIn(r, rng)
	}
	return points
}

// randomPointIn 在区间内均匀取一个点。用无符号运算算跨度，避免两端极值相减溢出。
func randomPointIn(r BlockRange, rng *rand.Rand) int64 {
	span := uint64(r.Hi) - uint64(r.Lo)
	return int64(uint64(r.Lo) + randomOffset(span, rng))
}

// randomOffset 返回 [0, span] 内均匀分布的偏移量。
// 区间可能跨过 0 一直到正负极值（跨度接近 2^64），装不进 int63，因此分两种情况取随机数。
func randomOffset(span uint64, rng *rand.Rand) uint64 {
	if span == 0 {
		return 0
	}
	// 跨度不超过 int63-1 时，Int63n 的上界是安全的（span+1 <= 2^63-1）。
	// 恰好等于 int63 最大值时必须走 64 位分支：此时 int64(span)+1 会溢出成负数，
	// Int63n 收到非正参数会 panic。
	const maxInt63 = uint64(1<<63 - 1)
	if span < maxInt63 {
		return uint64(rng.Int63n(int64(span) + 1))
	}
	// 跨度超过 int63 能表示的范围：拼两个 63 位随机数得到 64 位随机值。
	v := uint64(rng.Int63())<<1 | uint64(rng.Int63()&1)
	if span == ^uint64(0) {
		return v
	}
	return v % (span + 1)
}

// PickSampleCandidates 从候选主键中随机选出 needed 个。
// 先去重再洗牌，候选不足时返回 false，调用方应降级为全量扫描。
func PickSampleCandidates(candidates []int64, needed int, rng *rand.Rand) ([]int64, bool) {
	if needed <= 0 {
		return nil, true
	}
	seen := make(map[int64]bool, len(candidates))
	unique := make([]int64, 0, len(candidates))
	for _, k := range candidates {
		if !seen[k] {
			seen[k] = true
			unique = append(unique, k)
		}
	}
	if len(unique) < needed {
		return nil, false
	}
	rng.Shuffle(len(unique), func(i, j int) { unique[i], unique[j] = unique[j], unique[i] })
	return unique[:needed], true
}

// OfferReservoir 是蓄水池抽样的单步判定（纯函数，便于单测）。
// seen 是当前已扫描行数（从 1 开始计数），key 是本次读到的主键，pool 是池子。
// 返回新的 seen 和池子。
func OfferReservoir(seen int64, key int64, capacity int, pool []int64, rng *rand.Rand) (int64, []int64) {
	seen++
	if len(pool) < capacity {
		pool = append(pool, key)
		return seen, pool
	}
	// 第 seen 行以 capacity/seen 的概率替换池中随机一项。
	j := rng.Int63n(seen)
	if j < int64(capacity) {
		pool[int(j)] = key
	}
	return seen, pool
}

// ShuffleAndSplit 把池子洗牌后切成 rounds 轮，每轮 size 条，轮间不重叠。
// 池子不足时，前面的轮先分到，后面的轮可能为空。
func ShuffleAndSplit(pool []int64, rounds, size int, rng *rand.Rand) [][]int64 {
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	out := make([][]int64, 0, rounds)
	pos := 0
	for r := 0; r < rounds; r++ {
		end := pos + size
		if end > len(pool) {
			end = len(pool)
		}
		round := make([]int64, end-pos)
		copy(round, pool[pos:end])
		out = append(out, round)
		pos = end
	}
	return out
}

// SampleKeys 从归档表抽样，返回按轮切分的主键清单。
// 条件模式下归档表只装命中数据，直接在归档表上抽样即可。
func SampleKeys(ctx context.Context, db *sql.DB, table, keyCol string, rounds, size int, seed int64) ([][]int64, error) {
	return SampleKeysWithCondition(ctx, db, table, keyCol, "", rounds, size, seed)
}

// sampleWhere 把附加过滤条件拼成 WHERE 片段。condition 由工具内部构造，可信。
func sampleWhere(condition string) string {
	if condition == "" {
		return ""
	}
	return " WHERE (" + condition + ")"
}

// maxProbeRanges 是分区取点的区间上限。每个区间最多贡献一个候选，
// 因此样本数一旦超过它，分区路径必然凑不齐候选、一定退化为流式扫描。
const maxProbeRanges = 10000

// SampleScanFallbackNotice 生成"退化为流式扫描"的提示。
// needed 是需要的样本总数，candidates 是分区路径实际取到的候选数。
func SampleScanFallbackNotice(needed, candidates int) string {
	reason := "表较小或主键空洞较多，分区取点凑不够样本"
	if needed > maxProbeRanges {
		reason = fmt.Sprintf("样本数 %d 超过分区取点上限 %d，候选最多只有 %d 条",
			needed, maxProbeRanges, maxProbeRanges)
	}
	return fmt.Sprintf("注意: 本次抽样退化为流式扫描（%s，实际取到 %d 个候选）："+
		"数据库会逐行返回归档表全部主键；程序里只保留抽中的 %d 行、不会全部驻留内存，"+
		"但大表上传输和扫描会明显变慢。如需避免，把 sample_rounds × sample_size 控制在 %d 以内。",
		reason, candidates, needed, maxProbeRanges)
}

// SampleKeysWithCondition 在附加过滤条件下抽样。
// 删除续跑时传入"主键 > 上次删除位置"，只对尚未删除的部分做校验。
func SampleKeysWithCondition(ctx context.Context, db *sql.DB, table, keyCol, condition string, rounds, size int, seed int64) ([][]int64, error) {
	if rounds <= 0 || size <= 0 {
		return nil, fmt.Errorf("抽样轮数和每轮条数必须为正数")
	}
	needed := rounds * size
	rng := rand.New(rand.NewSource(seed))
	where := sampleWhere(condition)

	// 第一步：只读主键索引取范围，一次返回两个值，不扫描数据行。
	var lo, hi sql.NullInt64
	rangeSQL := fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s%s", qIdent(keyCol), qIdent(keyCol), qIdent(table), where)
	if err := db.QueryRowContext(ctx, rangeSQL).Scan(&lo, &hi); err != nil {
		return nil, fmt.Errorf("读取归档表主键范围失败: %w", err)
	}

	// 第二步：每个区间用随机探针取一条最近的候选。冗余系数 3 用于对抗主键空洞。
	var candidates []int64
	if lo.Valid && hi.Valid {
		// 区间数封顶 maxProbeRanges：配置里单轮上限 10000 × 轮数上限 100 × 3 会过大。
		// 每个区间最多出一个候选，因此样本数超过上限时分区路径必然凑不齐，走降级。
		rangeCount := needed * 3
		if rangeCount > maxProbeRanges {
			rangeCount = maxProbeRanges
		}
		ranges := MakeSampleRanges(lo.Int64, hi.Int64, rangeCount)
		probes := ProbePoints(ranges, rng)
		for i, r := range ranges {
			k, found, err := pickKeyNearProbe(ctx, db, table, keyCol, condition, r, probes[i])
			if err != nil {
				return nil, err
			}
			if found {
				candidates = append(candidates, k)
			}
		}
	}

	chosen, ok := PickSampleCandidates(candidates, needed, rng)
	if ok {
		return ShuffleAndSplit(chosen, rounds, size, rng), nil
	}
	// 候选不足：小表或空洞极多，或样本数超过区间上限（此时必然不足），
	// 降级为原来的流式蓄水池扫描。降级必须显式提示，不能静默换路径。
	fmt.Println(SampleScanFallbackNotice(needed, len(candidates)))
	return sampleKeysByScan(ctx, db, table, keyCol, where, rounds, size, rng)
}

// pickKeyNearProbe 在区间内取离探针最近的一条主键：先向后找（>= 探针），
// 探针落在区间最后一个键之后时再向前回退一次。这样只要区间内有数据就一定取到候选，
// 取到哪一条由探针决定，换种子就换候选。
func pickKeyNearProbe(ctx context.Context, db *sql.DB, table, keyCol, condition string, r BlockRange, probe int64) (int64, bool, error) {
	where := keyRangeWhere(keyCol, condition, probe, r.Hi)
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT 1",
		qIdent(keyCol), qIdent(table), where, qIdent(keyCol))
	var k int64
	err := db.QueryRowContext(ctx, q).Scan(&k)
	if err == nil {
		return k, true, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("分区取点失败（区间 [%d,%d]，探针 %d）: %w", r.Lo, r.Hi, probe, err)
	}
	if probe <= r.Lo {
		return 0, false, nil
	}
	// 向后没有数据：向前回退，取区间内小于探针的最后一个键。
	backWhere := keyRangeWhere(keyCol, condition, r.Lo, probe-1)
	back := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s DESC LIMIT 1",
		qIdent(keyCol), qIdent(table), backWhere, qIdent(keyCol))
	err = db.QueryRowContext(ctx, back).Scan(&k)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("分区取点失败（区间 [%d,%d]，探针 %d）: %w", r.Lo, r.Hi, probe, err)
	}
	return k, true, nil
}

// keyRangeWhere 拼出"主键落在 [lo,hi] 且满足迁移条件"的 WHERE 片段。
func keyRangeWhere(keyCol, condition string, lo, hi int64) string {
	clause := fmt.Sprintf("%s BETWEEN %d AND %d", qIdent(keyCol), lo, hi)
	if condition != "" {
		clause = "(" + condition + ") AND " + clause
	}
	return clause
}

// sampleKeysByScan 是原全量流式扫描，仅在分区取点候选不足时使用。
func sampleKeysByScan(ctx context.Context, db *sql.DB, table, keyCol, where string, rounds, size int, rng *rand.Rand) ([][]int64, error) {
	capacity := rounds * size
	q := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s", qIdent(keyCol), qIdent(table), where, qIdent(keyCol))
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("抽样扫描主键失败: %w", err)
	}
	defer rows.Close()

	var seen int64
	pool := make([]int64, 0, capacity)
	for rows.Next() {
		var key int64
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		seen, pool = OfferReservoir(seen, key, capacity, pool, rng)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ShuffleAndSplit(pool, rounds, size, rng), nil
}

// BuildSampleWhereByKey 构造续跑校验用的归档表过滤条件：只看主键大于续跑起点的行。
func BuildSampleWhereByKey(keyCol string, startKey int64) string {
	if startKey <= 0 {
		return ""
	}
	return fmt.Sprintf("%s > %d", qIdent(keyCol), startKey)
}
