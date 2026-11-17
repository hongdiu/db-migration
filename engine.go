package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"time"
)

// baseline 是在迁移前从源表采集的核对基线。
type baseline struct {
	count    int64
	sums     map[string]*ExactNumber
	nonempty map[string]int64
	minmax   map[string]*MinMaxValue
	snaps    [][]*RowSnapshot
	kinds    map[string]NumericKind
	// schemaHash 是采集基线时源表的列结构指纹，用于事后复核时确认“表没被改过”。
	// 没有它，一次 DDL 之后用旧基线核对新表结构，可能得出看似正常的错误结论。
	schemaHash string
	// capturedAt 是基线采集时刻，verify-only 时写进报告，提示这不是实时采集。
	capturedAt time.Time
	// sourcePath 是基线文件路径，仅 verify-only 读取文件基线时设置。
	sourcePath string
	// sampleSeed 是抽样用的随机种子，写进报告便于复现同一批样本。
	sampleSeed int64
}

// captureBaseline 在源表上（可带 condition）采集核对基线。
func captureBaseline(ctx context.Context, db *sql.DB, table, condition string, tc *TableConfig) (*baseline, error) {
	cols, err := tableColumns(ctx, db, table)
	if err != nil {
		return nil, err
	}
	if err := validateVerifyFields(cols, &tc.Verify); err != nil {
		return nil, err
	}
	kinds := columnKindMap(cols)
	v := tc.Verify
	b := &baseline{kinds: kinds, schemaHash: computeSchemaHash(cols), capturedAt: time.Now()}

	if v.UseRowCount() {
		b.count, err = countRows(ctx, db, table, condition)
		if err != nil {
			return nil, fmt.Errorf("统计基线记录数失败: %w", err)
		}
	}
	b.sums, err = sumFields(ctx, db, table, condition, v.NumericSumFields, kinds)
	if err != nil {
		return nil, fmt.Errorf("统计基线求和失败: %w", err)
	}
	if v.NonemptyCount {
		b.nonempty, err = nonemptyCounts(ctx, db, table, condition, v.NonemptyFields)
		if err != nil {
			return nil, fmt.Errorf("统计基线非空数失败: %w", err)
		}
	}
	if v.MinMax {
		b.minmax, err = minMaxFields(ctx, db, table, condition, v.MinMaxFields, kinds)
		if err != nil {
			return nil, fmt.Errorf("统计基线最值失败: %w", err)
		}
	}
	if v.SampleRounds > 0 && v.SampleSize > 0 {
		seed := time.Now().UnixNano()
		rng := rand.New(rand.NewSource(seed))
		b.sampleSeed = seed
		snaps, err := takeSnapshots(ctx, db, table, tc.KeyColumn, condition, v.SampleRounds, v.SampleSize, rng)
		if err != nil {
			return nil, fmt.Errorf("抽样失败: %w", err)
		}
		b.snaps = snaps
	}
	return b, nil
}

// validateVerifyFields 校验核对字段是否真实存在且类型合适。
// 重点是求和字段必须是数值类型：旧实现对非数值列执行 SUM 时 MySQL 会隐式按 0 处理，
// 解析成小数后“成功通过”，属于静默漏检，必须在采集阶段直接报错。
func validateVerifyFields(cols []ColumnMeta, v *VerifyConfig) error {
	byName := make(map[string]ColumnMeta, len(cols))
	for _, c := range cols {
		byName[strings.ToLower(c.Name)] = c
	}
	check := func(fields []string, what string, requireNumeric bool) error {
		for _, f := range fields {
			c, ok := byName[strings.ToLower(f)]
			if !ok {
				return fmt.Errorf("%s字段 %q 在表中不存在", what, f)
			}
			if requireNumeric && c.Kind == NumericNone {
				return fmt.Errorf("%s字段 %q 的数据库类型是 %s，不是数值类型，不能用于求和核对",
					what, f, strings.ToUpper(c.DataType))
			}
		}
		return nil
	}
	if err := check(v.NumericSumFields, "求和核对", true); err != nil {
		return err
	}
	if err := check(v.NonemptyFields, "非空比对", false); err != nil {
		return err
	}
	if err := check(v.MinMaxFields, "最值比对", false); err != nil {
		return err
	}
	return nil
}

// verifyAgainst 在历史表上采集同样指标并与基线比对，结果写入报告。
// historyCondition：历史表核对时使用的条件（条件迁移用空串，因为历史表只含本次记录）。
func verifyAgainst(ctx context.Context, db *sql.DB, tc *TableConfig, historyTable string, base *baseline, rep *TableReport, historyCondition string) error {
	v := tc.Verify
	kinds := base.kinds
	cfg := NumericCompareConfig{AbsTol: v.AbsTolerance, RelTol: v.RelTolerance}

	if v.UseRowCount() {
		n, err := countRows(ctx, db, historyTable, historyCondition)
		if err != nil {
			return fmt.Errorf("统计历史表记录数失败: %w", err)
		}
		rep.addWithRule("记录数比对", n == base.count,
			fmt.Sprintf("迁移前 %d / 迁移后 %d", base.count, n), "记录数精确相等")
	}

	if len(v.NumericSumFields) > 0 {
		after, err := sumFields(ctx, db, historyTable, historyCondition, v.NumericSumFields, kinds)
		if err != nil {
			return fmt.Errorf("统计历史表求和失败: %w", err)
		}
		for _, f := range v.NumericSumFields {
			bv := base.sums[f]
			av := after[f]
			if bv == nil || av == nil {
				rep.addWithRule("SUM("+f+")", false, "缺少基线或迁移后求和值", "")
				continue
			}
			pass, detail := bv.Compare(av, cfg)
			rule := "整数/定点小数零容差精确相等"
			if bv.Kind == NumericFloat {
				rule = fmt.Sprintf("浮点双阈值容差(abs=%g,rel=%g)", cfg.AbsTol, cfg.RelTol)
			}
			rep.addWithRule("SUM("+f+")", pass,
				fmt.Sprintf("%s；迁移前=%s 迁移后=%s", detail, bv.Raw, av.Raw), rule)
		}
	}

	if v.NonemptyCount {
		after, err := nonemptyCounts(ctx, db, historyTable, historyCondition, v.NonemptyFields)
		if err != nil {
			return fmt.Errorf("统计历史表非空数失败: %w", err)
		}
		for _, f := range v.NonemptyFields {
			pass := after[f] == base.nonempty[f]
			rep.addWithRule("非空数("+f+")", pass,
				fmt.Sprintf("迁移前 %d / 迁移后 %d", base.nonempty[f], after[f]), "非空记录数精确相等")
		}
	}

	if v.MinMax {
		after, err := minMaxFields(ctx, db, historyTable, historyCondition, v.MinMaxFields, kinds)
		if err != nil {
			return fmt.Errorf("统计历史表最值失败: %w", err)
		}
		for _, f := range v.MinMaxFields {
			bv, av := base.minmax[f], after[f]
			if bv == nil || av == nil {
				rep.addWithRule("最值("+f+")", false, "缺少基线或迁移后最值", "")
				continue
			}
			pass := compareMinMax(bv, av, cfg)
			rep.addWithRule("最值("+f+")", pass,
				fmt.Sprintf("迁移前[min=%s,max=%s] 迁移后[min=%s,max=%s]",
					bv.MinText, bv.MaxText, av.MinText, av.MaxText), "最小值与最大值分别比对")
		}
	}

	// 抽样逐字段比对。
	for i, snaps := range base.snaps {
		name := fmt.Sprintf("抽样第%d轮", i+1)
		if len(snaps) == 0 {
			rep.addWithRule(name, true, "无数据可抽样", "")
			continue
		}
		diffs, err := compareSnapshots(ctx, db, historyTable, tc.KeyColumn, snaps, cfg, tc.Name, i+1)
		if err != nil {
			return fmt.Errorf("%s对比失败: %w", name, err)
		}
		rep.Diffs = append(rep.Diffs, diffs...)
		rule := "逐字段类型感知比对(数值精确/容差,NULL严格区分)；蓄水池抽样，按行等概率"
		if base.sampleSeed != 0 {
			rule += fmt.Sprintf("，随机种子=%d", base.sampleSeed)
		}
		rep.addWithRule(name, len(diffs) == 0,
			fmt.Sprintf("抽样 %d 条, 通过 %d, 失败 %d", len(snaps), len(snaps)-countDistinctKeys(diffs), countDistinctKeys(diffs)),
			rule)
	}
	return nil
}

func countDistinctKeys(diffs []FieldDiff) int {
	m := make(map[int64]bool)
	for _, d := range diffs {
		m[d.Key] = true
	}
	return len(m)
}

func compareMinMax(a, b *MinMaxValue, cfg NumericCompareConfig) bool {
	if a.Kind != NumericNone && a.MinNum != nil && b.MinNum != nil {
		if pass, _ := a.MinNum.Compare(b.MinNum, cfg); !pass {
			return false
		}
	} else if a.MinText != b.MinText {
		return false
	}
	if a.Kind != NumericNone && a.MaxNum != nil && b.MaxNum != nil {
		if pass, _ := a.MaxNum.Compare(b.MaxNum, cfg); !pass {
			return false
		}
	} else if a.MaxText != b.MaxText {
		return false
	}
	return true
}
