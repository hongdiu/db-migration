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
	Results          []CheckResult
	Fields           []ReviewField
	SampleRounds     [][]int64 // 抽中的主键（按轮），用于复查 SQL 输出
	KeyCol           string    // 探测到的真实主键列名
	ArchiveCondition string    // 归档表附加过滤条件（续跑时排除已删部分）
	Seed             int64     // 本次抽样种子，写入复查 SQL 便于复现
}

// RunVerify 执行全量校验（只读），verify 动作使用。
// seed 决定抽样种子：每次执行传不同值即可扩大抽查面，复查 SQL 会记录该种子便于复现。
func RunVerify(ctx context.Context, db *sql.DB, t *TableCfg, seed int64) (*VerifyOutcome, error) {
	return RunVerifyRange(ctx, db, t, 0, seed)
}

// RunVerifyRange 校验归档表中主键大于 startKey 的部分与源表剩余数据是否一致。
// 删除续跑时传入上次删除到的主键，避免把已经删掉的部分算进校验。
// verify 和 purge 共用本函数，保证人工核对与删除门禁用的是同一套判定。
func RunVerifyRange(ctx context.Context, db *sql.DB, t *TableCfg, startKey, seed int64) (*VerifyOutcome, error) {
	// 主键探测（分块与删除门禁都依赖整型主键）。
	pk, err := probePrimaryKey(ctx, db, t.Name)
	if err != nil {
		return nil, err
	}
	keyCol, err := judgePrimaryKeyUsable(t.KeyColumn, pk)
	if err != nil {
		return nil, err
	}
	archiveCond := BuildSampleWhereByKey(keyCol, startKey)

	var results []CheckResult

	// 1. 记录数：源表命中数 vs 归档表行数。
	srcCount, err := countRows(ctx, db, t.Name, t.Condition)
	if err != nil {
		return nil, fmt.Errorf("统计源表行数失败: %w", err)
	}
	archiveCount, err := countRows(ctx, db, t.ArchiveTable, archiveCond)
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
		dstSum, err := aggregateValue(ctx, db, "SUM", f.Name, t.ArchiveTable, keyCol, archiveCond, nil)
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
			dstV, err := aggregateValue(ctx, db, opName, f.Name, t.ArchiveTable, keyCol, archiveCond, nil)
			if err != nil {
				return nil, err
			}
			ok2, detail2 := CompareNumbers(srcV, dstV, f.Kind, t.Verify.AbsTolerance, t.Verify.RelTolerance)
			results = append(results, CheckResult{Label: fmt.Sprintf("%s %s", f.Name, opName), Pass: ok2, Detail: detail2})
		}
	}

	// 4. 主键分块计数：用归档表的主键区间切块，逐块比对，定位差异落在哪一段。
	// 续跑时只取尚未删除的区间作为边界，跳过已删部分。
	var lo, hi sql.NullInt64
	rangeSQL := fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s%s", qIdent(keyCol), qIdent(keyCol), qIdent(t.ArchiveTable), sampleWhere(archiveCond))
	if err := db.QueryRowContext(ctx, rangeSQL).Scan(&lo, &hi); err != nil {
		return nil, fmt.Errorf("读取归档表主键范围失败: %w", err)
	}
	if lo.Valid && hi.Valid {
		blockSize := FitBlockSize(lo.Int64, hi.Int64, t.Verify.BlockSize, t.Verify.BlockCountMax)
		if blockSize != t.Verify.BlockSize {
			fmt.Printf("表 %s 主键跨度较大，分块大小由 %d 放大到 %d（分块数上限 %d），差异定位精度相应变粗。\n",
				t.Name, t.Verify.BlockSize, blockSize, t.Verify.BlockCountMax)
		}
		for _, block := range MakeBlocks(lo.Int64, hi.Int64, blockSize) {
			sc, err := countRowsInBlock(ctx, db, t.Name, keyCol, t.Condition, block)
			if err != nil {
				return nil, err
			}
			dc, err := countRowsInBlock(ctx, db, t.ArchiveTable, keyCol, archiveCond, block)
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
		// 种子由调用方传入：verify/purge 每次执行用不同种子，复查 SQL 记录种子便于复现。
		sampled, err := SampleKeysWithCondition(ctx, db, t.ArchiveTable, keyCol, archiveCond, t.Verify.Rounds(), t.Verify.PerRound(), seed)
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

	return &VerifyOutcome{Results: results, Fields: fields, SampleRounds: sampleRounds, KeyCol: keyCol, ArchiveCondition: archiveCond, Seed: seed}, nil
}

// DoSample 执行独立抽样动作：用传入种子抽一批主键，逐字段比对两表，出复查 SQL。
// 与 verify 里的抽样共用底层函数，但种子由调用方决定（默认每次不同），
// 可反复执行以扩大抽查面；verify 里的固定种子抽样不受影响。
func DoSample(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg, seed int64) error {
	// 探测主键与列：抽样比对依赖整型单列主键和两侧字段类型。
	pk, err := probePrimaryKey(ctx, db, t.Name)
	if err != nil {
		return err
	}
	keyCol, err := judgePrimaryKeyUsable(t.KeyColumn, pk)
	if err != nil {
		return err
	}

	rounds := t.Verify.Rounds()
	size := t.Verify.PerRound()
	if !t.Verify.SampleEnabled() {
		return fmt.Errorf("抽样未开启：verify.sample_rounds 与 verify.sample_size 都必须大于 0")
	}
	sampled, err := SampleKeys(ctx, db, t.ArchiveTable, keyCol, rounds, size, seed)
	if err != nil {
		return err
	}
	cols, kinds, err := sampleColumns(ctx, db, t.Name)
	if err != nil {
		return err
	}

	results := make([]CheckResult, 0, len(sampled))
	for r, keys := range sampled {
		verdict, err := CompareSampleRound(ctx, db, t.Name, t.ArchiveTable, keyCol, cols, kinds, keys, t.Verify.AbsTolerance, t.Verify.RelTolerance)
		if err != nil {
			return err
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

	sqlText := BuildSampleReviewSQL(t.Name, t.ArchiveTable, t.Condition, keyCol, ResultsToReviewItems(results), sampled, seed, t.Verify.AbsTolerance, t.Verify.RelTolerance)
	if _, err := OutputSampleSQL(cfg.Output.Dir, t.Name, seed, sqlText); err != nil {
		return err
	}
	if !AllPassed(results) {
		return fmt.Errorf("抽样核对未通过，请在上面的复查 SQL 中对照判定为[不通过]的轮次排查；源表未做任何改动")
	}
	fmt.Printf("表 %s 抽样核对全部通过（%d 轮 × 每轮 %d 行，种子 %d）。\n", t.Name, rounds, size, seed)
	return nil
}

// DoVerify 执行校验动作：跑校验 + 出复查 SQL（文件 + 控制台）。
func DoVerify(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg, seed int64) error {
	outcome, err := RunVerify(ctx, db, t, seed)
	if err != nil {
		return err
	}
	results := outcome.Results
	sqlText := BuildReviewSQLWithArchiveCondition(t.Name, t.ArchiveTable, t.Condition, outcome.ArchiveCondition, outcome.KeyCol, outcome.Fields,
		ResultsToReviewItems(results), outcome.SampleRounds, outcome.Seed, t.Verify.AbsTolerance, t.Verify.RelTolerance)
	if _, err := OutputReviewSQL(cfg.Output.Dir, t.Name, sqlText); err != nil {
		return err
	}
	if !AllPassed(results) {
		return fmt.Errorf("校验未通过，请在上面的复查 SQL 中对照工具判定为[不通过]的项目排查；源表未做任何改动")
	}
	fmt.Printf("表 %s 校验全部通过（%d 项）。\n", t.Name, len(results))
	return nil
}

// hasNewWrites 检查是否有新数据落入归档范围。
// 只看主键大于已归档最大主键的行，命中一条立即返回，走主键索引极快。
func hasNewWrites(ctx context.Context, db *sql.DB, table, condition, keyCol string, maxArchived sql.NullInt64) (bool, error) {
	if !maxArchived.Valid {
		return false, nil
	}
	q := BuildNewWriteCheckQuery(table, condition, keyCol, maxArchived.Int64)
	var one int
	err := db.QueryRowContext(ctx, q).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// JudgePurgeSamplingAllowed 判定是否允许在没有抽样的情况下执行删除。
// 抽样是发现"两行数据互换"这类行级错位的唯一手段：记录数、求和、最值、分块计数
// 在对称错位时会全部相等，只有逐字段回读能抓出来。因此抽样被配置关掉时默认拒绝删除，
// 必须显式加 -force 承担漏检风险。
func JudgePurgeSamplingAllowed(sampleEnabled, force bool) error {
	if sampleEnabled {
		return nil
	}
	if force {
		return nil
	}
	return fmt.Errorf("抽样已关闭（verify.sample_rounds 或 sample_size 为 0），purge 默认拒绝执行：" +
		"抽样是发现\"两行数据互换\"这类行级错位的唯一手段，记录数/求和/最值/分块计数在这种情况下会全部相等。" +
		"请把 verify.sample_rounds 与 verify.sample_size 设为大于 0 后重跑，或加 -force 明确承担漏检风险")
}

// RunPurge 执行删除动作：强制先校验，全部通过才按已校验主键清单分批删。
// 支持中途中断后从进度文件续删；每批只按实际删除行数递减计数，不再重复全表统计。
func RunPurge(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg, seed int64, force bool) error {
	if err := JudgePurgeSamplingAllowed(t.Verify.SampleEnabled(), force); err != nil {
		return err
	}
	if !t.Verify.SampleEnabled() {
		fmt.Println("警告: 抽样已关闭，行级错位只能靠记录数/求和/分块计数发现，可能漏检；已按 -force 继续。")
	}
	fingerprint := ConfigFingerprint(*t)
	cp, err := LoadCheckpoint(cfg.Checkpoint.Dir, t.Name)
	if err != nil {
		return err
	}
	if cp != nil && cp.Stage == "purged" && cp.Fingerprint == fingerprint {
		fmt.Printf("表 %s 已标记为删除完成，跳过；如需重做请删除进度文件。\n", t.Name)
		return nil
	}
	progress, err := JudgePurgeProgress(cp, fingerprint)
	if err != nil {
		return err
	}
	startKey := progress.StartKey
	deletedSoFar := progress.DeletedBefore
	if progress.Resuming {
		fmt.Printf("表 %s 从断点续删：起点主键 %d，此前已删 %d 行。\n", t.Name, startKey, deletedSoFar)
	}

	// 删除前强制复核：续跑时只校验尚未删除的部分，已删部分不参与比对。
	outcome, err := RunVerifyRange(ctx, db, t, startKey, seed)
	if err != nil {
		return err
	}
	results := outcome.Results
	if !AllPassed(results) {
		sqlText := BuildReviewSQLWithArchiveCondition(t.Name, t.ArchiveTable, t.Condition, outcome.ArchiveCondition, outcome.KeyCol, nil,
			ResultsToReviewItems(results), outcome.SampleRounds, outcome.Seed, t.Verify.AbsTolerance, t.Verify.RelTolerance)
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

	// 开始前先看一眼有没有新数据落入归档范围，保住原有告警能力。
	newWrite, err := hasNewWrites(ctx, db, t.Name, t.Condition, keyCol, maxArchived)
	if err != nil {
		return err
	}
	if newWrite {
		return fmt.Errorf("删除前检查发现源表有新数据落入归档范围（主键大于已归档最大值），请先重新搬运并校验，再执行删除")
	}

	// 待删总数只查一次，之后用每批实际删除行数递减，不再每批全表计数。
	srcRemaining, err := countRows(ctx, db, t.Name, t.Condition)
	if err != nil {
		return err
	}
	lastKey := startKey
	var copyLastKey, copyRowsDone int64
	if cp != nil {
		copyLastKey, copyRowsDone = cp.LastKey, cp.RowsDone
	}

	for {
		// 从归档表取一批已校验的主键（这些就是允许删除的范围）。
		rows, err := db.QueryContext(ctx, BuildArchivedKeysQuery(t.ArchiveTable, keyCol, lastKey, t.BatchSize))
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
		// 实际删除数与计划不符，说明数据被外部改动，立即停止，不继续带错往下删。
		if affected != int64(len(keys)) {
			return fmt.Errorf("本批计划删 %d 行，实际删 %d 行（数据可能被外部改动），已停止", len(keys), affected)
		}
		srcRemaining, deletedSoFar = ApplyPurgeBatch(srcRemaining, deletedSoFar, affected)
		lastKey = keys[len(keys)-1]

		// 原子写续跑断点：搬运信息原样保留，追加删除进度。
		next := &Checkpoint{
			Fingerprint:  fingerprint,
			Table:        t.Name,
			LastKey:      copyLastKey,
			RowsDone:     copyRowsDone,
			PurgeLastKey: lastKey,
			PurgeDeleted: deletedSoFar,
			Stage:        "purging",
		}
		if err := SaveCheckpoint(cfg.Checkpoint.Dir, t.Name, next); err != nil {
			return err
		}
		fmt.Printf("表 %s 已删 %d 行（累计 %d）。\n", t.Name, affected, deletedSoFar)
	}

	// 删完再看一眼，防止删除期间有新数据落入归档范围。
	newWrite, err = hasNewWrites(ctx, db, t.Name, t.Condition, keyCol, maxArchived)
	if err != nil {
		return err
	}
	if newWrite {
		return fmt.Errorf("删除期间发现新数据落入归档范围，已停止；请重新搬运并校验后再删")
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

	// 标记完成，避免误重跑时把已删数据当成新任务。
	done := &Checkpoint{
		Fingerprint:  fingerprint,
		Table:        t.Name,
		LastKey:      copyLastKey,
		RowsDone:     copyRowsDone,
		PurgeLastKey: lastKey,
		PurgeDeleted: deletedSoFar,
		Stage:        "purged",
	}
	return SaveCheckpoint(cfg.Checkpoint.Dir, t.Name, done)
}
