// 抽样行的逐字段比对。
//
// 对抽中的每个主键，分别回读源表和归档表的全部字段，逐字段比对。
// 这能抓出求和/计数发现不了的行级错位：比如两行数据互换，两边 SUM 一样、COUNT 一样，
// 但抽中其中一行回读时，该行的每个字段都对不上。
//
// 比对规则：
//   - 数值列按类型：整数/定点小数零容差，浮点绝对+相对双容差（复用 CompareNumbers）。
//   - 非数值列（文本、日期等）：按字符串精确比对。
//   - NULL 与 0、空串严格区分；生成列跳过（两边都不能显式插入）。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// RowDiff 是单个字段的差异明细。
type RowDiff struct {
	Key    int64
	Column string
	Src    string
	Dst    string
	Reason string
}

// CompareSampledRow 比对单个主键在两张表的全部字段。
// srcRow / dstRow 都是 列名 -> 值 的映射，值用 NumericValue 保留 NULL 语义。
// kinds 是列名 -> 数值类别（非数值列为 NumericNone）。
// 返回该行的差异列表，空表示完全一致。
func CompareSampledRow(key int64, columns []string, kinds map[string]NumericKind, srcRow, dstRow map[string]NumericValue, absTol, relTol float64) []RowDiff {
	var diffs []RowDiff
	for _, col := range columns {
		sv, sok := srcRow[col]
		dv, dok := dstRow[col]
		if !sok || !dok {
			diffs = append(diffs, RowDiff{Key: key, Column: col, Reason: "列缺失（源或目标）"})
			continue
		}
		kind := kinds[col] // 非数值列为 NumericNone，CompareNumbers 走字符串精确比对
		ok, reason := CompareNumbers(sv, dv, kind, absTol, relTol)
		if !ok {
			diffs = append(diffs, RowDiff{Key: key, Column: col, Src: displayValue(sv), Dst: displayValue(dv), Reason: reason})
		}
	}
	return diffs
}

func displayValue(v NumericValue) string {
	if v.IsNull {
		return "NULL"
	}
	return v.Raw
}

// readRow 按主键回读一张表的全部字段。
func readRow(ctx context.Context, db *sql.DB, table string, columns []string, keyCol string, key int64) (map[string]NumericValue, error) {
	colList := strings.Join(columns, ",")
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s = %d", colList, qIdent(table), qIdent(keyCol), key)
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	row := make(map[string]NumericValue)
	if !rows.Next() {
		return row, nil // 该主键在表中不存在，交给比对逻辑报"列缺失"
	}
	vals := make([]sql.NullString, len(columns))
	ptrs := make([]any, len(columns))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	for i, col := range columns {
		// columns 里的列名带反引号（拼 SQL 用），map 的 key 统一用裸列名，
		// 与 CompareSampledRow / unquoteColumns 的查找方式保持一致。
		name := strings.Trim(col, "`")
		if vals[i].Valid {
			row[name] = TextValue(vals[i].String)
		} else {
			row[name] = NullValue()
		}
	}
	return row, rows.Err()
}

// sampleColumns 返回参与逐字段比对的列（跳过生成列）及类型映射。
func sampleColumns(ctx context.Context, db *sql.DB, table string) ([]string, map[string]NumericKind, error) {
	cols, err := tableColumns(ctx, db, table)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	kinds := make(map[string]NumericKind, len(cols))
	for _, c := range cols {
		if isGeneratedColumn(c.Extra) {
			continue
		}
		names = append(names, qIdent(c.Name))
		kinds[c.Name] = NumericKindFromDBType(c.DataType)
	}
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("表 %s 没有可比对的列", table)
	}
	return names, kinds, nil
}

// SampleVerdict 是一轮抽样的比对结果汇总。
type SampleVerdict struct {
	Round   int
	Pass    bool
	Diffs   []RowDiff
	Sampled int // 本轮抽样行数
}

// CompareSampleRound 比对一轮抽样：逐主键回读两表全部字段并比对。
func CompareSampleRound(ctx context.Context, db *sql.DB, source, archive, keyCol string, columns []string, kinds map[string]NumericKind, keys []int64, absTol, relTol float64) (SampleVerdict, error) {
	v := SampleVerdict{Round: 0, Pass: true, Sampled: len(keys)}
	for _, key := range keys {
		srcRow, err := readRow(ctx, db, source, columns, keyCol, key)
		if err != nil {
			return v, fmt.Errorf("回读源表主键 %d 失败: %w", key, err)
		}
		dstRow, err := readRow(ctx, db, archive, columns, keyCol, key)
		if err != nil {
			return v, fmt.Errorf("回读归档表主键 %d 失败: %w", key, err)
		}
		diffs := CompareSampledRow(key, unquoteColumns(columns), kinds, srcRow, dstRow, absTol, relTol)
		if len(diffs) > 0 {
			v.Pass = false
			v.Diffs = append(v.Diffs, diffs...)
		}
	}
	return v, nil
}

// unquoteColumns 去掉列名外的反引号，得到裸列名用于 map 查找和差异展示。
func unquoteColumns(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = strings.Trim(c, "`")
	}
	return out
}
