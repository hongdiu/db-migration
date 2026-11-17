package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// 本文件解决“复制期间源表被业务写入，工具毫无感知”的问题。
//
// 条件迁移的复制窗口可能很长（千万级表按批复制）。期间业务若更新了已复制的行、
// 或插入了满足条件但主键小于当前进度的行，历史表拿到的是旧状态或干脆缺行。
// 而核对又是拿“复制前基线”去比历史表，最坏情况是两边“恰好一致”通过，
// 或者报一个看不出原因的失败。这里在复制结束后重新采一次轻量指纹与基线比对，
// 把“复制窗口内发生过写入”这件事明确写进结论。
//
// 需要说清的边界：这是“发现变化”，不是“追平变化”。要精确追平必须依赖 binlog/CDC
// 或触发器，属于另一套机制，不在本工具范围内。

// driftFingerprint 是源表在某一时刻的轻量指纹。
// 刻意不含抽样快照：检测只关心“有没有变化”，不需要逐行比对，成本必须与核对解耦。
type driftFingerprint struct {
	count    int64
	sums     map[string]*ExactNumber
	nonempty map[string]int64
	minmax   map[string]*MinMaxValue
}

// captureDriftFingerprint 采集源表当前指纹，只覆盖配置里已启用的核对项。
func captureDriftFingerprint(ctx context.Context, db *sql.DB, table, condition string, tc *TableConfig) (*driftFingerprint, error) {
	cols, err := tableColumns(ctx, db, table)
	if err != nil {
		return nil, err
	}
	kinds := columnKindMap(cols)
	v := tc.Verify
	f := &driftFingerprint{}
	if v.UseRowCount() {
		if f.count, err = countRows(ctx, db, table, condition); err != nil {
			return nil, fmt.Errorf("统计源表记录数失败: %w", err)
		}
	}
	if f.sums, err = sumFields(ctx, db, table, condition, v.NumericSumFields, kinds); err != nil {
		return nil, fmt.Errorf("统计源表求和失败: %w", err)
	}
	if v.NonemptyCount {
		if f.nonempty, err = nonemptyCounts(ctx, db, table, condition, v.NonemptyFields); err != nil {
			return nil, fmt.Errorf("统计源表非空数失败: %w", err)
		}
	}
	if v.MinMax {
		if f.minmax, err = minMaxFields(ctx, db, table, condition, v.MinMaxFields, kinds); err != nil {
			return nil, fmt.Errorf("统计源表最值失败: %w", err)
		}
	}
	return f, nil
}

// driftDiffs 比对复制前后的指纹，返回人类可读的差异项列表。空列表表示未发现变化。
func driftDiffs(base *baseline, now *driftFingerprint, v *VerifyConfig) []string {
	if base == nil || now == nil {
		return nil
	}
	var diffs []string
	if v.UseRowCount() && base.count != now.count {
		diffs = append(diffs, fmt.Sprintf("记录数 %d → %d", base.count, now.count))
	}
	cfg := NumericCompareConfig{AbsTol: v.AbsTolerance, RelTol: v.RelTolerance}
	for _, f := range v.NumericSumFields {
		bv, av := base.sums[f], now.sums[f]
		if bv == nil || av == nil {
			continue
		}
		if pass, _ := bv.Compare(av, cfg); !pass {
			diffs = append(diffs, fmt.Sprintf("SUM(%s) %s → %s", f, bv.Raw, av.Raw))
		}
	}
	for _, f := range v.NonemptyFields {
		if base.nonempty[f] != now.nonempty[f] {
			diffs = append(diffs, fmt.Sprintf("非空数(%s) %d → %d", f, base.nonempty[f], now.nonempty[f]))
		}
	}
	for _, f := range v.MinMaxFields {
		bv, av := base.minmax[f], now.minmax[f]
		if bv == nil || av == nil {
			continue
		}
		if !compareMinMax(bv, av, cfg) {
			diffs = append(diffs, fmt.Sprintf("最值(%s) [%s,%s] → [%s,%s]",
				f, bv.MinText, bv.MaxText, av.MinText, av.MaxText))
		}
	}
	return diffs
}

// checkSourceDrift 在复制结束后检测源表是否在复制窗口内发生变化，并把结论写入报告。
// 检测到变化记为失败项：此时历史表不是源表的完整快照，后续清理必须先处理这个问题。
func checkSourceDrift(ctx context.Context, db *sql.DB, tc *TableConfig, base *baseline, rep *TableReport) error {
	v := &tc.Verify
	if !v.UseRowCount() && len(v.NumericSumFields) == 0 && !v.NonemptyCount && !v.MinMax {
		// 没有任何可比对的指标时检测无从谈起，明确说明而不是给出一个“通过”。
		rep.addWithRule("源表复制期间变化", true, "未配置可检测项，已跳过检测",
			"至少需要 row_count 或求和/非空数/最值字段之一")
		return nil
	}
	now, err := captureDriftFingerprint(ctx, db, tc.Name, tc.Condition, tc)
	if err != nil {
		return err
	}
	diffs := driftDiffs(base, now, v)
	if len(diffs) == 0 {
		rep.addWithRule("源表复制期间变化", true, "复制前后源表指纹一致",
			"记录数/求和/非空数/最值逐一比对，未发现写入")
		return nil
	}
	rep.addWithRule("源表复制期间变化", false, strings.Join(diffs, "；"),
		"复制窗口内源表被写入，历史表不是源表的完整快照；请核对业务侧改动，"+
			"必要时在低峰窗口重跑（可配置 verify.detect_source_drift: false 关闭检测）")
	return nil
}
