// 执行层：把校验、搬运、删除串成三个动作的实际流程。
package main

import (
	"context"
	"database/sql"
	"fmt"
)

// buildReviewFields 从表元数据中找出配置了数值校验的列及其类型。
func buildReviewFields(ctx context.Context, db *sql.DB, table string, configured []string) ([]ReviewField, error) {
	cols, err := tableColumns(ctx, db, table)
	if err != nil {
		return nil, err
	}
	kindByName := make(map[string]NumericKind, len(cols))
	for _, c := range cols {
		kindByName[c.Name] = NumericKindFromDBType(c.DataType)
	}
	var fields []ReviewField
	for _, name := range configured {
		kind, ok := kindByName[name]
		if !ok {
			return nil, fmt.Errorf("校验字段 %s 在表 %s 中不存在", name, table)
		}
		if kind == NumericNone {
			return nil, fmt.Errorf("校验字段 %s 的类型不是数值类型，不能做数值校验", name)
		}
		fields = append(fields, ReviewField{Name: name, Kind: kind})
	}
	return fields, nil
}

// VerifyOutcome 是一次完整校验的全部产出。
type VerifyOutcome struct {
	Results      []CheckResult
	Fields       []ReviewField
	SampleRounds [][]int64 // 抽中的主键（按轮），用于复查 SQL 输出
	KeyCol       string    // 探测到的真实主键列名
}

// RunVerify 执行完整校验（只读）。
// verify 和 purge 共用本函数，保证人工核对与删除门禁用的是同一套判定。
func RunVerify(ctx context.Context, db *sql.DB, t *TableCfg) (*VerifyOutcome, error) {
	// 主键探测（分块与删除门禁都依赖整型主键）。
	pk, err := probePrimaryKey(ctx, db, t.Name)
	if err != nil {
		return nil, err
	}
	keyCol, err := judgePrimaryKeyUsable(t.KeyColumn, pk)
	if err != nil {
		return nil, err
	}

	var results []CheckResult

	// 1. 记录数：源表命中数 vs 归档表行数。
	srcCount, err := countRows(ctx, db, t.Name, t.Condition)
	if err != nil {
		return nil, fmt.Errorf("统计源表行数失败: %w", err)
	}
	archiveCount, err := countRows(ctx, db, t.ArchiveTable, "")
	if err != nil {
		return nil, fmt.Errorf("统计归档表行数失败: %w", err)
	}
	results = append(results, JudgeCounts("记录数", srcCount, archiveCount, BlockRange{}))

	// 2. 字段类型探测。
	fields, err := buildReviewFields(ctx, db, t.Name, t.Verify.NumericFields)
	if err != nil {
		return nil, err
	}

	// 3. 数值列求和与最值（按字段类型分派比对方式）。
	for _, f := range fields {
		srcSum, err := aggregateValue(ctx, db, "SUM", f.Name, t.Name, keyCol, t.Condition, nil)
		if err != nil {
			return nil, err
		}
		dstSum, err := aggregateValue(ctx, db, "SUM", f.Name, t.ArchiveTable, keyCol, "", nil)
		if err != nil {
			return nil, err
		}
		ok, detail := CompareNumbers(srcSum, dstSum, f.Kind, t.Verify.AbsTolerance, t.Verify.RelTolerance)
		results = append(results, CheckResult{Label: fmt.Sprintf("%s 求和", f.Name), Pass: ok, Detail: detail})

		for _, opName := range []string{"MIN", "MAX"} {
			srcV, err := aggregateValue(ctx, db, opName, f.Name, t.Name, keyCol, t.Condition, nil)
			if err != nil {
				return nil, err
			}
			dstV, err := aggregateValue(ctx, db, opName, f.Name, t.ArchiveTable, keyCol, "", nil)
			if err != nil {
				return nil, err
			}
			ok2, detail2 := CompareNumbers(srcV, dstV, f.Kind, t.Verify.AbsTolerance, t.Verify.RelTolerance)
			results = append(results, CheckResult{Label: fmt.Sprintf("%s %s", f.Name, opName), Pass: ok2, Detail: detail2})
		}
	}

	// 4. 主键分块计数：用归档表的主键区间切块，逐块比对，定位差异落在哪一段。
	var lo, hi sql.NullInt64
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s", qIdent(keyCol), qIdent(keyCol), qIdent(t.ArchiveTable))).Scan(&lo, &hi); err != nil {
		return nil, fmt.Errorf("读取归档表主键范围失败: %w", err)
	}
	if lo.Valid && hi.Valid {
		for _, block := range MakeBlocks(lo.Int64, hi.Int64, t.Verify.BlockSize) {
			sc, err := countRowsInBlock(ctx, db, t.Name, keyCol, t.Condition, block)
			if err != nil {
				return nil, err
			}
			dc, err := countRowsInBlock(ctx, db, t.ArchiveTable, keyCol, "", block)
			if err != nil {
				return nil, err
			}
			results = append(results, JudgeCounts(
				fmt.Sprintf("分块[%d,%d]计数", block.Lo, block.Hi), sc, dc, block))
		}
	}

	// 5. 随机抽样逐字段比对：抓求和/计数发现不了的行级错位。
	var sampleRounds [][]int64
	if t.Verify.SampleEnabled() {
		// 抽样种子固定为 0，保证同一批数据每次校验抽到相同的行，便于复现和人工核对。
		sampled, err := SampleKeys(ctx, db, t.ArchiveTable, keyCol, t.Verify.Rounds(), t.Verify.PerRound(), 0)
		if err != nil {
			return nil, err
		}
		sampleRounds = sampled
		cols, kinds, err := sampleColumns(ctx, db, t.Name)
		if err != nil {
			return nil, err
		}
		for r, keys := range sampled {
			verdict, err := CompareSampleRound(ctx, db, t.Name, t.ArchiveTable, keyCol, cols, kinds, keys, t.Verify.AbsTolerance, t.Verify.RelTolerance)
			if err != nil {
				return nil, err
			}
			verdict.Round = r + 1
			cr := CheckResult{Label: fmt.Sprintf("抽样第%d轮逐字段比对（%d行）", verdict.Round, verdict.Sampled), Pass: verdict.Pass}
			if verdict.Pass {
				cr.Detail = fmt.Sprintf("%d 行全部字段一致", verdict.Sampled)
			} else {
				cr.Detail = fmt.Sprintf("%d 行中发现 %d 处字段差异", verdict.Sampled, len(verdict.Diffs))
				for _, d := range verdict.Diffs {
					cr.Detail += fmt.Sprintf("\n    主键%d 字段%s：源=%s 目标=%s（%s）", d.Key, d.Column, d.Src, d.Dst, d.Reason)
				}
			}
			results = append(results, cr)
		}
	}

	return &VerifyOutcome{Results: results, Fields: fields, SampleRounds: sampleRounds, KeyCol: keyCol}, nil
}

// DoVerify 执行校验动作：跑校验 + 出复查 SQL（文件 + 控制台）。
func DoVerify(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg) error {
	outcome, err := RunVerify(ctx, db, t)
	if err != nil {
		return err
	}
	results := outcome.Results
	sqlText := BuildReviewSQL(t.Name, t.ArchiveTable, t.Condition, outcome.KeyCol, outcome.Fields,
		ResultsToReviewItems(results), outcome.SampleRounds, t.Verify.AbsTolerance, t.Verify.RelTolerance)
	if _, err := OutputReviewSQL(cfg.Output.Dir, t.Name, sqlText); err != nil {
		return err
	}
	if !AllPassed(results) {
		return fmt.Errorf("校验未通过，请在上面的复查 SQL 中对照工具判定为[不通过]的项目排查；源表未做任何改动")
	}
	fmt.Printf("表 %s 校验全部通过（%d 项）。\n", t.Name, len(results))
	return nil
}

// RunPurge 执行删除动作：强制先校验，全部通过才按已校验主键清单分批删。
func RunPurge(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg) error {
	// 删除前强制复核，不通过直接返回，源表不动。
	outcome, err := RunVerify(ctx, db, t)
	if err != nil {
		return err
	}
	results := outcome.Results
	if !AllPassed(results) {
		sqlText := BuildReviewSQL(t.Name, t.ArchiveTable, t.Condition, outcome.KeyCol, nil,
			ResultsToReviewItems(results), outcome.SampleRounds, t.Verify.AbsTolerance, t.Verify.RelTolerance)
		_, _ = OutputReviewSQL(cfg.Output.Dir, t.Name, sqlText)
		return fmt.Errorf("删除前校验未通过，已停止；源表未做任何改动，请按复查 SQL 排查")
	}

	// 主键探测。
	pk, err := probePrimaryKey(ctx, db, t.Name)
	if err != nil {
		return err
	}
	keyCol, err := judgePrimaryKeyUsable(t.KeyColumn, pk)
	if err != nil {
		return err
	}

	archiveRows, err := countRows(ctx, db, t.ArchiveTable, "")
	if err != nil {
		return err
	}
	var maxArchived sql.NullInt64
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT MAX(%s) FROM %s", qIdent(keyCol), qIdent(t.ArchiveTable))).Scan(&maxArchived); err != nil {
		return err
	}

	var deletedSoFar int64
	var lastKey int64
	for {
		// 从归档表取一批已校验的主键（这些就是允许删除的范围）。
		rows, err := db.QueryContext(ctx,
			fmt.Sprintf("SELECT %s FROM %s WHERE %s > %d ORDER BY %s LIMIT %d",
				qIdent(keyCol), qIdent(t.ArchiveTable), qIdent(keyCol), lastKey, qIdent(keyCol), t.BatchSize))
		if err != nil {
			return err
		}
		var keys []int64
		for rows.Next() {
			var k int64
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(keys) == 0 {
			break
		}

		srcRemaining, err := countRows(ctx, db, t.Name, t.Condition)
		if err != nil {
			return err
		}
		verdict := PurgeVerdict{
			SrcRowsToDelete: srcRemaining,
			DeletedSoFar:    deletedSoFar,
			ArchiveRows:     archiveRows,
			BatchRows:       int64(len(keys)),
		}
		if err := JudgePurgeAllowed(verdict); err != nil {
			return fmt.Errorf("删除守恒校验失败，已停止: %w", err)
		}

		delSQL := BuildDeleteByKeys(t.Name, keyCol, keys)
		res, err := db.ExecContext(ctx, delSQL)
		if err != nil {
			return fmt.Errorf("删除批次失败: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		deletedSoFar += affected
		lastKey = keys[len(keys)-1]
		fmt.Printf("表 %s 已删 %d 行（累计 %d）。\n", t.Name, affected, deletedSoFar)
	}

	// 删后残留必须为 0（限定在已归档主键范围内）。
	if maxArchived.Valid {
		remainSQL := BuildRemainingCountQuery(t.Name, keyCol, t.Condition, maxArchived.Int64)
		var remain int64
		if err := db.QueryRowContext(ctx, remainSQL).Scan(&remain); err != nil {
			return err
		}
		if remain != 0 {
			return fmt.Errorf("删除后源表仍有 %d 行命中数据，请排查", remain)
		}
	}
	return nil
}
