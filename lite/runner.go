// 执行层：把校验、搬运、删除串成三个动作的实际流程。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
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
	tols := t.Verify.Tolerances()

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
	results = append(results, JudgeCounts("记录数", srcCount, archiveCount))

	// 2. 字段类型探测。
	fields, err := buildReviewFields(ctx, db, t.Name, t.Verify.NumericFields)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		// 不能静默降级：不配数值列时，金额类差异只剩记录数、分块计数和抽样三道兜底，
		// 结论里必须说清楚，否则"校验全部通过"会被误读成"数值也对过了"。
		fmt.Printf("注意: 表 %s 未配置 verify.numeric_fields，本次跳过所有数值列的求和、最值与非空数校验；"+
			"金额类数据只能靠记录数、分块计数和抽样发现差异。\n", t.Name)
	}

	// 3. 数值列求和、最值与非空数（按字段类型分派比对方式）。
	for _, f := range fields {
		srcSum, err := aggregateValue(ctx, db, "SUM", f.Name, t.Name, keyCol, t.Condition, nil)
		if err != nil {
			return nil, err
		}
		dstSum, err := aggregateValue(ctx, db, "SUM", f.Name, t.ArchiveTable, keyCol, archiveCond, nil)
		if err != nil {
			return nil, err
		}
		ok, detail := CompareNumbers(srcSum, dstSum, f.Kind, tols)
		results = append(results, CheckResult{Label: fmt.Sprintf("%s 求和", f.Name), Pass: ok, Detail: detail})

		// 非空数：SUM 相等但一侧是 NULL、另一侧是 0 时，只有这一项能区分。
		srcNN, err := aggregateValue(ctx, db, "COUNT", f.Name, t.Name, keyCol, t.Condition, nil)
		if err != nil {
			return nil, err
		}
		dstNN, err := aggregateValue(ctx, db, "COUNT", f.Name, t.ArchiveTable, keyCol, archiveCond, nil)
		if err != nil {
			return nil, err
		}
		results = append(results, JudgeNonNullCounts(fmt.Sprintf("%s 非空数", f.Name), srcNN, dstNN))

		for _, opName := range []string{"MIN", "MAX"} {
			srcV, err := aggregateValue(ctx, db, opName, f.Name, t.Name, keyCol, t.Condition, nil)
			if err != nil {
				return nil, err
			}
			dstV, err := aggregateValue(ctx, db, opName, f.Name, t.ArchiveTable, keyCol, archiveCond, nil)
			if err != nil {
				return nil, err
			}
			ok2, detail2 := CompareNumbers(srcV, dstV, f.Kind, tols)
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
				fmt.Sprintf("分块[%d,%d]计数", block.Lo, block.Hi), sc, dc))
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
			verdict, err := CompareSampleRound(ctx, db, t.Name, t.ArchiveTable, keyCol, cols, kinds, keys, tols)
			if err != nil {
				return nil, err
			}
			verdict.Round = r + 1
			cr := CheckResult{Label: fmt.Sprintf("抽样第%d轮逐字段比对（%d行）", verdict.Round, verdict.Sampled), Pass: verdict.Pass}
			if verdict.Pass {
				cr.Detail = fmt.Sprintf("%d 行所有可比对列一致（生成列除外）", verdict.Sampled)
			} else {
				cr.Detail = fmt.Sprintf("%d 行中发现 %d 处差异", verdict.Sampled, len(verdict.Diffs))
				cr.Detail += formatSampleDiffs(verdict.Diffs)
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

	// 部分 purge 之后，已删的那批主键只留在归档表里。照旧从整个归档表抽，
	// 会抽到源表已不存在的主键，回读时被误报成"列缺失"。按删除断点限定抽样范围。
	cp, err := LoadCheckpoint(cfg.Checkpoint.Dir, t.Name)
	if err != nil {
		return err
	}
	if cp != nil && cp.Stage == "purged" {
		fmt.Printf("表 %s 已标记为删除完成，源表已无数据可抽样，跳过。\n", t.Name)
		return nil
	}
	startKey := int64(0)
	if cp != nil && cp.Stage == "purging" {
		startKey = cp.PurgeLastKey
	}
	archiveCond := BuildSampleWhereByKey(keyCol, startKey)
	if startKey > 0 {
		fmt.Printf("表 %s 已有删除进度，本次只对主键大于 %d 的部分抽样，避免抽到已删行。\n", t.Name, startKey)
	}

	rounds := t.Verify.Rounds()
	size := t.Verify.PerRound()
	if !t.Verify.SampleEnabled() {
		return fmt.Errorf("抽样未开启：verify.sample_rounds 与 verify.sample_size 都必须大于 0")
	}
	tols := t.Verify.Tolerances()
	sampled, err := SampleKeysWithCondition(ctx, db, t.ArchiveTable, keyCol, archiveCond, rounds, size, seed)
	if err != nil {
		return err
	}
	cols, kinds, err := sampleColumns(ctx, db, t.Name)
	if err != nil {
		return err
	}

	results := make([]CheckResult, 0, len(sampled))
	for r, keys := range sampled {
		verdict, err := CompareSampleRound(ctx, db, t.Name, t.ArchiveTable, keyCol, cols, kinds, keys, tols)
		if err != nil {
			return err
		}
		verdict.Round = r + 1
		cr := CheckResult{Label: fmt.Sprintf("抽样第%d轮逐字段比对（%d行）", verdict.Round, verdict.Sampled), Pass: verdict.Pass}
		if verdict.Pass {
			cr.Detail = fmt.Sprintf("%d 行所有可比对列一致（生成列除外）", verdict.Sampled)
		} else {
			cr.Detail = fmt.Sprintf("%d 行中发现 %d 处差异", verdict.Sampled, len(verdict.Diffs))
			cr.Detail += formatSampleDiffs(verdict.Diffs)
		}
		results = append(results, cr)
	}

	sqlText := BuildSampleReviewSQLWithArchiveCondition(t.Name, t.ArchiveTable, t.Condition, archiveCond, keyCol, ResultsToReviewItems(results), sampled, seed, tols)
	if _, err := OutputSampleSQL(cfg.Output.Dir, t.Name, seed, sqlText); err != nil {
		return err
	}
	if !AllPassed(results) {
		return fmt.Errorf("抽样核对未通过，请在上面的复查 SQL 中对照判定为[不通过]的轮次排查；源表未做任何改动")
	}
	fmt.Printf("表 %s 抽样核对全部通过（%d 轮 × 每轮 %d 行，种子 %d）。\n", t.Name, rounds, size, seed)
	return nil
}

// formatSampleDiffs 把一轮抽样的差异明细拼成报告文字。
// 整行缺失（Column 为空）和字段值不同是两类问题，文案要分开，避免把"主键已被删掉"说成"列缺失"。
func formatSampleDiffs(diffs []RowDiff) string {
	var sb strings.Builder
	for _, d := range diffs {
		if d.Column == "" {
			sb.WriteString(fmt.Sprintf("\n    主键%d：%s", d.Key, d.Reason))
			continue
		}
		sb.WriteString(fmt.Sprintf("\n    主键%d 字段%s：源=%s 目标=%s（%s）", d.Key, d.Column, d.Src, d.Dst, d.Reason))
	}
	return sb.String()
}

// verifyConclusion 生成"校验通过"时的结论文案。
// 未配置数值列时必须显式点出缺口：这类配置下金额差异只能靠记录数、分块和抽样兜底，
// 一句笼统的"全部通过"会让人误以为数值列也核对过了。
func verifyConclusion(table string, checkedItems, numericFields int, sampleEnabled bool) string {
	if numericFields == 0 && !sampleEnabled {
		return fmt.Sprintf("表 %s 校验通过（%d 项），但未配置 verify.numeric_fields 且抽样已关闭：数值列未做任何比对，"+
			"行级错位也未做逐字段抽查，这个结论既不代表金额类数据已被核对，也不代表做过抽样。", table, checkedItems)
	}
	if numericFields == 0 {
		return fmt.Sprintf("表 %s 校验通过（%d 项），但未配置 verify.numeric_fields：数值列未做任何比对，"+
			"这个结论不代表金额类数据已被核对。", table, checkedItems)
	}
	if !sampleEnabled {
		return fmt.Sprintf("表 %s 校验通过（%d 项），但抽样已关闭：未做逐字段抽查，行级错位可能漏检，"+
			"这个结论不代表做过抽样。", table, checkedItems)
	}
	return fmt.Sprintf("表 %s 校验全部通过（%d 项）。", table, checkedItems)
}

// DoVerify 执行校验动作：跑校验 + 出复查 SQL（文件 + 控制台）。
func DoVerify(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg, seed int64) error {
	outcome, err := RunVerify(ctx, db, t, seed)
	if err != nil {
		return err
	}
	results := outcome.Results
	sqlText := BuildReviewSQLWithArchiveCondition(t.Name, t.ArchiveTable, t.Condition, outcome.ArchiveCondition, outcome.KeyCol, outcome.Fields,
		ResultsToReviewItems(results), outcome.SampleRounds, outcome.Seed, t.Verify.Tolerances())
	if _, err := OutputReviewSQL(cfg.Output.Dir, t.Name, sqlText); err != nil {
		return err
	}
	if !AllPassed(results) {
		return fmt.Errorf("校验未通过，请在上面的复查 SQL 中对照工具判定为[不通过]的项目排查；源表未做任何改动")
	}
	if !t.Verify.SampleEnabled() {
		fmt.Println(SamplingDisabledNotice(t.Name, "verify"))
	}
	fmt.Println(verifyConclusion(t.Name, len(results), len(outcome.Fields), t.Verify.SampleEnabled()))
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

// SamplingDisabledNotice 生成"抽样已关闭"的统一提示。
// verify 与 purge 共用同一段说明，避免两处口径不一致。
func SamplingDisabledNotice(table, action string) string {
	return fmt.Sprintf("注意: 表 %s 本次 %s 未开启抽样（verify.sample_rounds 或 sample_size 为 0）："+
		"行级错位（如两行数据互换）只能靠记录数、求和、最值、非空数、分块计数发现，可能漏检；"+
		"结论不代表做过逐字段抽查。", table, action)
}

// JudgePurgeSamplingAllowed 判定是否允许在没有抽样的情况下执行删除。
// 抽样是发现"两行数据互换"这类行级错位的唯一手段：记录数、求和、最值、非空数、分块计数
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
		"抽样是发现\"两行数据互换\"这类行级错位的唯一手段，记录数/求和/最值/非空数/分块计数在这种情况下会全部相等。" +
		"请加 -force 明确承担漏检风险后重跑。" +
		"若要恢复抽样：抽样参数参与配置指纹，调整后旧删除进度会失效，必须重新搬运、重新校验")
}

// buildPurgeFailureReviewSQL 生成"删除前校验未通过"时的复查 SQL。
// 必须带上 outcome.Fields：拦下删除的可能正是某个数值列的求和、最值或非空数，
// 报告里缺了这些核对项，人拿到 SQL 也无法照着复查。
func buildPurgeFailureReviewSQL(t *TableCfg, outcome *VerifyOutcome) string {
	return BuildReviewSQLWithArchiveCondition(t.Name, t.ArchiveTable, t.Condition, outcome.ArchiveCondition,
		outcome.KeyCol, outcome.Fields, ResultsToReviewItems(outcome.Results), outcome.SampleRounds,
		outcome.Seed, t.Verify.Tolerances())
}

// RunPurge 执行删除动作：强制先校验，全部通过才按已校验主键清单分批删。
// 支持中途中断后从进度文件续删；每批只按实际删除行数递减计数，不再重复全表统计。
//
// 每批的落盘顺序是"意图 → 删除 → 确认"：
//   - 意图记录本批要删的区间和行数，此时进度仍停在这批之前；
//   - DELETE 提交后再写一次进度，把这批并入 PurgeLastKey 并清掉意图。
//
// 这样即使进程在 DELETE 之后、确认写入之前被杀，重跑也能凭"源表区间内剩余行数"
// 判断这批到底删没删，不必依赖"删除 + 写进度"在同一个事务里
// （一批几万行的 DELETE 放进事务代价太高）。
func RunPurge(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg, seed int64, force bool) error {
	fingerprint := ConfigFingerprint(*t)
	cp, err := LoadCheckpoint(cfg.Checkpoint.Dir, t.Name)
	if err != nil {
		return err
	}
	// 已完成的表先返回：否则抽样被关掉时，重跑一张早就删完的表会先撞上抽样门禁并报错。
	if cp != nil && cp.Stage == "purged" && cp.Fingerprint == fingerprint {
		fmt.Printf("表 %s 已标记为删除完成，跳过；如需重做请删除进度文件。\n", t.Name)
		return nil
	}
	if err := JudgePurgeSamplingAllowed(t.Verify.SampleEnabled(), force); err != nil {
		if notice := ForceAuditNoticeFrom(cfg.Checkpoint.Dir, t.Name, ForceScenePurgeSampling, fingerprint); notice != "" {
			fmt.Println(notice)
		}
		return err
	}
	if !t.Verify.SampleEnabled() {
		if err := RecordForce(cfg.Checkpoint.Dir, t.Name, ForceScenePurgeSampling, fingerprint, time.Now()); err != nil {
			return err
		}
		fmt.Println(SamplingDisabledNotice(t.Name, "purge") + " 已按 -force 继续。")
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

	// 主键探测提前到这里：恢复未确认批次时要用它统计区间剩余行数。
	pk, err := probePrimaryKey(ctx, db, t.Name)
	if err != nil {
		return err
	}
	keyCol, err := judgePrimaryKeyUsable(t.KeyColumn, pk)
	if err != nil {
		return err
	}

	// 恢复"已发起但未确认"的批次：按源表在该区间还剩多少行判断删除是否已生效。
	if cp != nil && cp.PurgePendingRows > 0 {
		remaining, err := countRowsInKeyRange(ctx, db, t.Name, keyCol, t.Condition, cp.PurgePendingFrom, cp.PurgePendingTo)
		if err != nil {
			return fmt.Errorf("核对上次未确认的删除批次失败: %w", err)
		}
		rec, err := JudgePendingPurge(cp, remaining)
		if err != nil {
			return err
		}
		if rec.Applied {
			startKey, deletedSoFar = rec.StartKey, rec.Deleted
			fmt.Printf("表 %s 上次删除的批次（主键 (%d, %d]，%d 行）已确认生效，进度已推进。\n",
				t.Name, cp.PurgePendingFrom, cp.PurgePendingTo, cp.PurgePendingRows)
		} else {
			fmt.Printf("表 %s 上次删除的批次（主键 (%d, %d]）未生效，重新执行这一批。\n",
				t.Name, cp.PurgePendingFrom, cp.PurgePendingTo)
		}
	}

	// 删除前强制复核：续跑时只校验尚未删除的部分，已删部分不参与比对。
	outcome, err := RunVerifyRange(ctx, db, t, startKey, seed)
	if err != nil {
		return err
	}
	results := outcome.Results
	if !AllPassed(results) {
		sqlText := buildPurgeFailureReviewSQL(t, outcome)
		_, _ = OutputReviewSQL(cfg.Output.Dir, t.Name, sqlText)
		return fmt.Errorf("删除前校验未通过，已停止；源表未做任何改动，请按复查 SQL 排查")
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

		// 先落删除意图：记录本批区间 (lastKey, keys[last]] 与计划行数，进度本体不动。
		// 写失败就还没删，直接返回，不留半截状态。
		intent := &Checkpoint{
			Fingerprint:      fingerprint,
			Table:            t.Name,
			LastKey:          copyLastKey,
			RowsDone:         copyRowsDone,
			PurgeLastKey:     lastKey,
			PurgeDeleted:     deletedSoFar,
			PurgePendingFrom: lastKey,
			PurgePendingTo:   keys[len(keys)-1],
			PurgePendingRows: int64(len(keys)),
			Stage:            "purging",
		}
		if err := SaveCheckpoint(cfg.Checkpoint.Dir, t.Name, intent); err != nil {
			return fmt.Errorf("写删除意图失败，本批未开始删除: %w", err)
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
			return fmt.Errorf("本批计划删 %d 行，实际删 %d 行（数据可能被外部改动），已停止；"+
				"进度文件里保留着本批意图，重跑会按源表剩余行数自动核对", len(keys), affected)
		}
		srcRemaining, deletedSoFar = ApplyPurgeBatch(srcRemaining, deletedSoFar, affected)
		lastKey = keys[len(keys)-1]

		// 确认落盘：把这批并入进度并清掉意图（pending 三个字段归零）。
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
