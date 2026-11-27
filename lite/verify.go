// 校验引擎：记录数、主键分块计数、数值列求和/最值/非空数。
//
// 设计原则：
//   - 校验对象是"每个分块两侧是否对齐"，不是"总数差不多"。总数相等但分块对不上，说明迁错了行。
//   - 分块结果既是校验项，也是后续删除的门禁：只有校验通过的分块才允许删。
//   - 所有判定走 CompareNumbers，按字段类型分派精确或容差比对。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
)

// CheckResult 是单个核对项的结果。
type CheckResult struct {
	Label   string // 核对项名称，如 "记录数"、"分块[0,99999]计数"
	Pass    bool
	Detail  string
	BlockLo int64 // 关联的主键分块下界；非分块项为 0
	BlockHi int64 // 关联的主键分块上界；非分块项为 0
}

// BlockRange 是一个主键分块区间 [lo, hi]。
type BlockRange struct {
	Lo int64
	Hi int64
}

// MakeBlocks 按 blockSize 把 [minKey, maxKey] 切成若干分块。
// minKey > maxKey（空表）时返回空切片。
func MakeBlocks(minKey, maxKey, blockSize int64) []BlockRange {
	if blockSize <= 0 || minKey > maxKey {
		return nil
	}
	var blocks []BlockRange
	for lo := minKey; lo <= maxKey; {
		hi := lo + blockSize - 1
		if hi < lo || hi > maxKey { // 防止整数溢出
			hi = maxKey
		}
		blocks = append(blocks, BlockRange{Lo: lo, Hi: hi})
		if hi == maxKey {
			break
		}
		lo = hi + 1
	}
	return blocks
}

// FitBlockSize 把分块大小调整到"分块数不超过 maxBlocks"，返回实际应使用的块宽。
// 主键跨度与分块数的关系是纯算术，放在这里便于单测：雪花 ID 这类跨度 1e18 的主键，
// 按固定 10 万切块会得到 1e13 个分块，任何机器都撑不住；此时放大块宽，宁可定位精度变粗。
// 返回原 blockSize 表示无需调整。
func FitBlockSize(minKey, maxKey, blockSize int64, maxBlocks int) int64 {
	if blockSize <= 0 || maxBlocks <= 0 || minKey >= maxKey {
		return blockSize
	}
	span := uint64(maxKey) - uint64(minKey) // 区间内键值个数 = span + 1
	// span/blockSize + 1 恒等于 ceil((span+1)/blockSize)，即按该块宽切出来的分块数。
	if span/uint64(blockSize)+1 <= uint64(maxBlocks) {
		return blockSize
	}
	count := span
	if count != ^uint64(0) {
		count = span + 1
	}
	per := count / uint64(maxBlocks)
	if count%uint64(maxBlocks) != 0 {
		per++
	}
	if per == 0 {
		per = 1
	}
	if per > uint64(math.MaxInt64) {
		// 极端情况（整段 int64 只切一块）：返回最大块宽，仍会让块数控制在个位数。
		return math.MaxInt64
	}
	return int64(per)
}

// JudgeCounts 判定两个计数是否相等（记录数和分块计数都是零容差）。
func JudgeCounts(label string, srcCount, dstCount int64, block BlockRange) CheckResult {
	r := CheckResult{Label: label, BlockLo: block.Lo, BlockHi: block.Hi}
	if srcCount == dstCount {
		r.Pass = true
		r.Detail = fmt.Sprintf("两侧均为 %d 行", srcCount)
	} else {
		r.Detail = fmt.Sprintf("行数不等：源=%d，归档=%d，差=%d", srcCount, dstCount, srcCount-dstCount)
	}
	return r
}

// JudgeNumericAggregate 按字段类型判定一个聚合值（求和或最值）。
func JudgeNumericAggregate(label string, src, dst string, kind NumericKind, tols Tolerances, nullable bool) CheckResult {
	r := CheckResult{Label: label}
	// 从数据库读出的 NULL 用 sql.NullString 语义识别：这里约定空表示 NULL 由调用方传入 IsNull。
	sv, dv := TextValue(src), TextValue(dst)
	ok, detail := CompareNumbers(sv, dv, kind, tols)
	r.Pass = ok
	r.Detail = detail
	if nullable {
		r.Detail += "（该列可空，NULL 已按独立值处理）"
	}
	return r
}

// JudgeNonNullCounts 判定两列的非空行数是否相等（零容差）。
// 这一项专门补 SUM/MIN/MAX 的盲区：一侧 100 行全为 0、另一侧 50 行为 0 加 50 行 NULL 时，
// SUM、MIN、MAX 完全相等，只有非空数和逐行抽样能区分。
func JudgeNonNullCounts(label string, src, dst NumericValue) CheckResult {
	r := CheckResult{Label: label}
	if src.IsNull || dst.IsNull {
		// COUNT(col) 不会返回 NULL；出现说明调用方传错了聚合函数。
		r.Detail = fmt.Sprintf("非空数异常：源=%s，归档=%s", nullLabel(src), nullLabel(dst))
		return r
	}
	if src.Raw == dst.Raw {
		r.Pass = true
		r.Detail = fmt.Sprintf("两侧非空行数均为 %s", src.Raw)
		return r
	}
	r.Detail = fmt.Sprintf("非空行数不等：源=%s，归档=%s（NULL 不算 0，也不能被 SUM 抵消）", src.Raw, dst.Raw)
	return r
}

// countRows 统计满足条件的行数。condition 为空则统计全表。
func countRows(ctx context.Context, db *sql.DB, table, condition string) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s", qIdent(table))
	if condition != "" {
		q += " WHERE " + condition
	}
	var n int64
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// countRowsInBlock 统计某个主键分块内的行数。
func countRowsInBlock(ctx context.Context, db *sql.DB, table, keyCol, condition string, block BlockRange) (int64, error) {
	where := fmt.Sprintf("%s BETWEEN %d AND %d", qIdent(keyCol), block.Lo, block.Hi)
	if condition != "" {
		where = "(" + condition + ") AND " + where
	}
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", qIdent(table), where)
	var n int64
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// countRowsInKeyRange 统计主键落在 (lo, hi] 内、且满足条件的行数。
// 删除意图记录的是左开右闭区间：起点是上一批已确认的终点，不能重复计入。
func countRowsInKeyRange(ctx context.Context, db *sql.DB, table, keyCol, condition string, lo, hi int64) (int64, error) {
	where := fmt.Sprintf("%s > %d AND %s <= %d", qIdent(keyCol), lo, qIdent(keyCol), hi)
	if condition != "" {
		where = "(" + condition + ") AND " + where
	}
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", qIdent(table), where)
	var n int64
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// aggregateValue 读取某列在给定范围的聚合值（SUM/MIN/MAX），保留原始字符串避免精度丢失。
// 聚合结果全为 NULL 时返回 IsNull=true。block 非空时额外限定主键区间。
func aggregateValue(ctx context.Context, db *sql.DB, op, column, table, keyCol, condition string, block *BlockRange) (NumericValue, error) {
	var clauses []string
	if condition != "" {
		clauses = append(clauses, "("+condition+")")
	}
	if block != nil {
		clauses = append(clauses, fmt.Sprintf("%s BETWEEN %d AND %d", qIdent(keyCol), block.Lo, block.Hi))
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	q := fmt.Sprintf("SELECT %s(%s) FROM %s%s", op, qIdent(column), qIdent(table), where)
	var raw sql.NullString
	if err := db.QueryRowContext(ctx, q).Scan(&raw); err != nil {
		return NumericValue{}, err
	}
	if !raw.Valid {
		return NullValue(), nil
	}
	return TextValue(raw.String), nil
}

// AllPassed 判断一组校验结果是否全部通过。
func AllPassed(results []CheckResult) bool {
	for _, r := range results {
		if !r.Pass {
			return false
		}
	}
	return true
}

// FailedBlocks 收集校验未通过的分块区间，作为删除门禁的排除项。
func FailedBlocks(results []CheckResult) []BlockRange {
	var out []BlockRange
	for _, r := range results {
		if !r.Pass && (r.BlockLo != 0 || r.BlockHi != 0) {
			out = append(out, BlockRange{Lo: r.BlockLo, Hi: r.BlockHi})
		}
	}
	return out
}

// ResultsToReviewItems 把校验结果转成复查 SQL 里的工具判定汇总项。
func ResultsToReviewItems(results []CheckResult) []ReviewItemResult {
	out := make([]ReviewItemResult, 0, len(results))
	for _, r := range results {
		v := "不通过"
		if r.Pass {
			v = "通过"
		}
		out = append(out, ReviewItemResult{Label: r.Label, Verdict: v, Detail: r.Detail})
	}
	return out
}
