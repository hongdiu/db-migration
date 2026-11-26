// 蓄水池抽样（Reservoir Sampling，Algorithm R）。
//
// 目的：从归档表按主键做一次顺序扫描，等概率抽出若干行，用于逐字段比对。
//
// 为什么用蓄水池：
//   - 无偏：每一行被抽中的概率相等，不会像"按主键区间取模"那样在主键有空洞时整段漏抽。
//   - 一次扫描：只读主键索引，成本 O(N)，可预测；不需要先 COUNT 再算随机偏移。
//   - 流式：不需要把全部主键装进内存，池子多大就占多少内存。
//
// 结果按轮切分：池容量 = rounds × size，扫描结束后洗牌，再切成 rounds 轮，轮间不重叠。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
)

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

// SampleKeys 从归档表按主键流式抽样，返回按轮切分的主键清单。
// 条件模式下归档表只装命中数据，直接在归档表上抽样即可。
func SampleKeys(ctx context.Context, db *sql.DB, table, keyCol string, rounds, size int, seed int64) ([][]int64, error) {
	if rounds <= 0 || size <= 0 {
		return nil, fmt.Errorf("抽样轮数和每轮条数必须为正数")
	}
	capacity := rounds * size
	q := fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", qIdent(keyCol), qIdent(table), qIdent(keyCol))
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("抽样扫描主键失败: %w", err)
	}
	defer rows.Close()

	rng := rand.New(rand.NewSource(seed))
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
