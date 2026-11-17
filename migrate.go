package dbmigration

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Options 是命令行选项。
type Options struct {
	ConfigPath string
	DryRun     bool
	Purge      bool   // 是否执行清理阶段（删除源表），默认 false
	VerifyOnly bool   // 仅核对
	Action     string // migrate | history
	HistoryOp  string // list | merge | purge
	Source     string // history 子命令的目标源表
	Confirm    bool   // history purge 的二次确认
}

// ---------------- 全表迁移 ----------------

func runFullTable(ctx context.Context, db *sql.DB, tc *TableConfig, cfg *MigrateConfig, opt Options) *TableReport {
	rep := &TableReport{Table: tc.Name, Mode: tc.Mode, DryRun: opt.DryRun, OutputDir: cfg.Output.Dir}
	historyTable := tc.Name + tc.HistorySuffix

	exists, err := tableExists(ctx, db, historyTable)
	if err != nil {
		rep.Err = fmt.Errorf("检查历史表失败: %w", err)
		return rep
	}

	// 仅核对必须放在“历史表已存在”的覆盖校验之前：
	// 全表迁移完成后历史表本来就在，若先做覆盖校验会被误判为冲突。
	if opt.VerifyOnly {
		if !exists {
			rep.Err = fmt.Errorf("历史表 %s 不存在，无法核对", historyTable)
			return rep
		}
		rep.Stage = "verify"
		rep.HistoryTbl = historyTable
		// 全表迁移后源表已是空表，基线无法从源表重建，只能依赖迁移前落盘的文件。
		// 读取时会校验基线的结构指纹与历史表是否一致，防止 DDL 之后用旧基线核对新结构。
		base, notice, lerr := loadBaselineForVerify(ctx, db, cfg.Checkpoint.Dir, tc, historyTable)
		if lerr != nil {
			rep.Err = fmt.Errorf("全表迁移后源表已为空表，无法重建基线；读取迁移前保存的基线失败: %w", lerr)
			return rep
		}
		rep.addWithRule("基线来源", true, notice,
			"文件基线，非实时采集；结构指纹已与历史表比对一致")
		if err := verifyAgainst(ctx, db, tc, historyTable, base, rep, ""); err != nil {
			rep.Err = err
		}
		return rep
	}

	if exists && !tc.OverwriteHistory {
		rep.Err = fmt.Errorf("历史表 %s 已存在，如需覆盖请设置 overwrite_history: true", historyTable)
		return rep
	}

	if opt.DryRun {
		base, err := captureBaseline(ctx, db, tc.Name, "", tc)
		if err != nil {
			rep.Err = err
			return rep
		}
		rep.addWithRule("迁移前记录数", true, fmt.Sprintf("%d", base.count), "dry-run 仅采集基线")
		for _, f := range tc.Verify.NumericSumFields {
			if n := base.sums[f]; n != nil {
				rep.addWithRule("SUM("+f+")", true, n.Raw, "dry-run 仅采集基线")
			}
		}
		return rep
	}

	// 采集迁移前基线（改名前，从源表读），并立即落盘。
	// 改名是原子的：一旦完成，源表就是空表，基线再也取不到，所以必须在改名之前保存。
	base, err := captureBaseline(ctx, db, tc.Name, "", tc)
	if err != nil {
		rep.Err = err
		return rep
	}
	if path, serr := saveBaseline(cfg.Checkpoint.Dir, tc, base); serr != nil {
		rep.addWithRule("保存基线", false, serr.Error(), "改名前必须落盘，否则事后无法复核")
	} else {
		rep.addWithRule("保存基线", true, path, "供 -verify-only 事后复核")
	}

	// 建新空表（结构与原表一致）。
	newTable := fmt.Sprintf("%s_mig_new_%s", tc.Name, time.Now().Format("20060102150405"))
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s LIKE %s", qIdent(newTable), qIdent(tc.Name))); err != nil {
		rep.Err = fmt.Errorf("创建新表失败: %w", err)
		return rep
	}
	cleanupNew := true
	defer func() {
		if cleanupNew {
			_, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+qIdent(newTable))
		}
	}()

	// 衔接自增主键，避免新表自增回退导致主键冲突。
	var maxKey sql.NullInt64
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT MAX(%s) FROM %s", qIdent(tc.KeyColumn), qIdent(tc.Name))).Scan(&maxKey); err != nil {
		rep.Err = fmt.Errorf("查询最大主键失败: %w", err)
		return rep
	}
	if maxKey.Valid {
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", qIdent(newTable), maxKey.Int64+1)); err != nil {
			rep.Err = fmt.Errorf("设置自增起点失败: %w", err)
			return rep
		}
	}

	// 原子改名：旧历史表->临时名，原表->历史表，新表->原表。
	if exists {
		trash := fmt.Sprintf("%s_trash_%s", historyTable, shortRand(8))
		sqlStmt := fmt.Sprintf("RENAME TABLE %s TO %s, %s TO %s, %s TO %s",
			qIdent(historyTable), qIdent(trash),
			qIdent(tc.Name), qIdent(historyTable),
			qIdent(newTable), qIdent(tc.Name))
		if _, err := db.ExecContext(ctx, sqlStmt); err != nil {
			rep.Err = fmt.Errorf("原子改名失败: %w", err)
			return rep
		}
		cleanupNew = false
		if _, err := db.ExecContext(ctx, "DROP TABLE "+qIdent(trash)); err != nil {
			rep.add("清理旧历史表", false, fmt.Sprintf("DROP %s 失败: %v（可手动清理）", trash, err))
		} else {
			rep.add("清理旧历史表", true, trash)
		}
	} else {
		sqlStmt := fmt.Sprintf("RENAME TABLE %s TO %s, %s TO %s",
			qIdent(tc.Name), qIdent(historyTable), qIdent(newTable), qIdent(tc.Name))
		if _, err := db.ExecContext(ctx, sqlStmt); err != nil {
			rep.Err = fmt.Errorf("原子改名失败: %w", err)
			return rep
		}
		cleanupNew = false
	}

	rep.Stage = "rename+verify"
	rep.HistoryTbl = historyTable
	rep.add("原子改名完成", true, fmt.Sprintf("%s -> %s，%s -> 新空表", tc.Name, historyTable, tc.Name))

	// 核对（历史表即迁移后数据）。
	if err := verifyAgainst(ctx, db, tc, historyTable, base, rep, ""); err != nil {
		rep.Err = err
		return rep
	}

	// 登记历史表，供生命周期管理。
	if err := registerHistory(ctx, db, tc, historyTable, base.count); err != nil {
		rep.add("登记历史表", false, err.Error())
	} else {
		rep.add("登记历史表", true, historyTable)
	}
	return rep
}

// ---------------- 条件迁移 ----------------

func historyTableName(tc *TableConfig, stamp string) string {
	if tc.UseHistoryTimestamp() {
		return tc.Name + tc.HistorySuffix + "_" + stamp
	}
	return tc.Name + tc.HistorySuffix
}

func runConditionalCopy(ctx context.Context, db *sql.DB, tc *TableConfig, cfg *MigrateConfig, opt Options, stamp string) *TableReport {
	rep := &TableReport{Table: tc.Name, Mode: tc.Mode, Stage: "copy", DryRun: opt.DryRun, OutputDir: cfg.Output.Dir}
	cond := tc.Condition

	if opt.VerifyOnly {
		return runConditionalVerifyOnly(ctx, db, tc, cfg, rep)
	}

	// 读取进度并校验（指纹匹配 + 历史表名一致）。
	cp, err := loadCheckpoint(cfg.Checkpoint.Dir, tc.Name, StageCopy)
	if err != nil {
		rep.Err = err
		return rep
	}

	var historyTable string
	var base *baseline
	lastKey, processed := int64(0), int64(0)
	resuming := cp != nil && cp.Fingerprint == fingerprint(tc) && cp.HistoryTable != ""

	if resuming {
		// 续跑：历史表名沿用进度里的，避免新建表或张冠李戴。
		historyTable = cp.HistoryTable
		lastKey = cp.LastKey
		processed = cp.ProcessedRows
		rep.HistoryTbl = historyTable
		rep.addWithRule("断点续跑", true,
			fmt.Sprintf("续用历史表 %s，从主键 %d 继续，已复制 %d 行", historyTable, lastKey, processed),
			"配置指纹与历史表名一致")
		// 续跑时源表仍未删除，基线可重新采集，保证核对基于当前真实数据。
		base, err = captureBaseline(ctx, db, tc.Name, cond, tc)
		if err != nil {
			rep.Err = err
			return rep
		}
	} else {
		historyTable = historyTableName(tc, stamp)
		base, err = captureBaseline(ctx, db, tc.Name, cond, tc)
		if err != nil {
			rep.Err = err
			return rep
		}
		if opt.DryRun {
			rep.addWithRule("待迁移记录数", true, fmt.Sprintf("%d", base.count), "dry-run 仅采集基线")
			for _, f := range tc.Verify.NumericSumFields {
				if n := base.sums[f]; n != nil {
					rep.addWithRule("待迁移 SUM("+f+")", true, n.Raw, "dry-run 仅采集基线")
				}
			}
			return rep
		}
		exists, err := tableExists(ctx, db, historyTable)
		if err != nil {
			rep.Err = err
			return rep
		}
		if exists {
			rep.Err = fmt.Errorf("历史表 %s 已存在，请更换批次时间戳或清理后重试", historyTable)
			return rep
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s LIKE %s", qIdent(historyTable), qIdent(tc.Name))); err != nil {
			rep.Err = fmt.Errorf("创建历史表失败: %w", err)
			return rep
		}
		var minKey sql.NullInt64
		if err := db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT MIN(%s) FROM %s WHERE (%s)", qIdent(tc.KeyColumn), qIdent(tc.Name), cond)).Scan(&minKey); err != nil {
			rep.Err = err
			return rep
		}
		if minKey.Valid {
			lastKey = minKey.Int64 - 1
		}
		cp = &Checkpoint{
			Table: tc.Name, Stage: string(StageCopy), Fingerprint: fingerprint(tc),
			HistoryTable: historyTable, LastKey: lastKey, ProcessedRows: 0,
			StartedAt: time.Now().Format(time.RFC3339), UpdatedAt: time.Now().Format(time.RFC3339),
		}
		if err := saveCheckpoint(cfg.Checkpoint.Dir, cp); err != nil {
			rep.Err = err
			return rep
		}
		rep.HistoryTbl = historyTable
	}

	// 复制前把基线落盘，供 -verify-only 事后复核。
	// 复制阶段源表不动，续跑时重新采集的基线同样有效。
	if path, serr := saveBaseline(cfg.Checkpoint.Dir, tc, base); serr != nil {
		rep.addWithRule("保存基线", false, serr.Error(), "供 -verify-only 事后复核")
	} else {
		rep.addWithRule("保存基线", true, path, "供 -verify-only 事后复核")
	}

	// 分批复制（源表不动）。
	if err := copyBatches(ctx, db, tc, cfg, historyTable, lastKey, processed, cp); err != nil {
		rep.Err = err
		return rep
	}

	// 复制窗口内源表可能被业务写入：重采轻量指纹与复制前基线比对，把这件事写进报告。
	if tc.Verify.UseSourceDriftCheck() {
		if err := checkSourceDrift(ctx, db, tc, base, rep); err != nil {
			rep.Err = err
			return rep
		}
	}

	// 复制完成后统一核对（新执行与续跑都走这里）。
	if err := verifyAgainst(ctx, db, tc, historyTable, base, rep, ""); err != nil {
		rep.Err = err
		return rep
	}

	// 登记历史表（记录数以历史表实际行数为准），避免重复登记。
	actual, err := countRows(ctx, db, historyTable, "")
	if err != nil {
		rep.Err = err
		return rep
	}
	if err := registerHistory(ctx, db, tc, historyTable, actual); err != nil {
		rep.addWithRule("登记历史表", false, err.Error(), "")
	} else {
		rep.addWithRule("登记历史表", true, historyTable, "")
	}

	if err := removeCheckpoint(cfg.Checkpoint.Dir, tc.Name, StageCopy); err != nil {
		rep.addWithRule("清理复制进度", false, err.Error(), "")
	}
	rep.addWithRule("复制阶段完成", true, "源表数据未做任何删除；如需删除请显式执行 --purge", "")
	return rep
}

// copyBatches 分批把满足条件的记录复制到历史表，源表不动。每批事务提交后写进度。
func copyBatches(ctx context.Context, db *sql.DB, tc *TableConfig, cfg *MigrateConfig, historyTable string, lastKey, processed int64, cp *Checkpoint) error {
	cols, err := insertableColumns(ctx, db, tc.Name)
	if err != nil {
		return err
	}
	colList := strings.Join(cols, ", ")
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		sel := fmt.Sprintf("SELECT %s FROM %s WHERE %s > ? AND (%s) ORDER BY %s LIMIT %d",
			qIdent(tc.KeyColumn), qIdent(tc.Name), qIdent(tc.KeyColumn),
			tc.Condition, qIdent(tc.KeyColumn), tc.BatchSize)
		rows, err := tx.QueryContext(ctx, sel, lastKey)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("查询待复制批次失败: %w", err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				tx.Rollback()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			tx.Rollback()
			return err
		}
		if len(ids) == 0 {
			tx.Rollback()
			break
		}
		next := ids[len(ids)-1]
		ph := make([]string, len(ids))
		args := make([]interface{}, len(ids))
		for i, id := range ids {
			ph[i] = "?"
			args[i] = id
		}
		// 这里刻意不用 INSERT IGNORE：
		// IGNORE 会把“字符串超长截断”“非空约束违规”这类错误降级为警告并继续写入，
		// 记录数与求和看起来都正常，坏数据却已经进了历史表，只有抽样才可能发现。
		// 改用普通 INSERT + ON DUPLICATE KEY UPDATE：
		//   - 遇到截断/约束违规会直接报错（配合启动时的严格模式校验），不再静默写坏数据；
		//   - 断点续跑重做同一批次时按幂等处理，不会因主键重复而中断。
		ins := fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s WHERE %s IN (%s) AND (%s)
			ON DUPLICATE KEY UPDATE %s = VALUES(%s)`,
			qIdent(historyTable), colList, colList, qIdent(tc.Name),
			qIdent(tc.KeyColumn), strings.Join(ph, ","), tc.Condition,
			qIdent(tc.KeyColumn), qIdent(tc.KeyColumn))
		if _, err := tx.ExecContext(ctx, ins, args...); err != nil {
			tx.Rollback()
			return fmt.Errorf("插入历史表失败: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		lastKey = next
		processed += int64(len(ids))
		cp.LastKey = lastKey
		cp.ProcessedRows = processed
		cp.UpdatedAt = time.Now().Format(time.RFC3339)
		if err := saveCheckpoint(cfg.Checkpoint.Dir, cp); err != nil {
			return fmt.Errorf("保存进度失败: %w", err)
		}
		fmt.Printf("  [%s] 已复制 %d 行\n", tc.Name, processed)
	}
	return nil
}

// activeConditionalHistoryTables 返回该源表所有“源表记录尚未清理”的条件迁移历史表。
// 旧实现只取最新一张，导致存在多个批次（不同时间戳）时旧批次永远清理不到。
func activeConditionalHistoryTables(ctx context.Context, db *sql.DB, sourceTable string) ([]HistoryEntry, error) {
	entries, err := listHistory(ctx, db, sourceTable)
	if err != nil {
		return nil, err
	}
	var out []HistoryEntry
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Mode != "conditional" || e.Status != statusActive {
			continue
		}
		// 按历史表名去重，防止登记表历史遗留的重复项把记录数重复累加。
		if seen[e.HistoryTable] {
			continue
		}
		seen[e.HistoryTable] = true
		out = append(out, e)
	}
	return out, nil
}

// latestArchivedHistoryTable 返回该源表最新一张已归档（active 或 purged）的条件迁移历史表。
// 供 -verify-only 使用：源表可能已经清理过，但历史表仍在保留期内。
func latestArchivedHistoryTable(ctx context.Context, db *sql.DB, sourceTable string) (string, error) {
	entries, err := listHistory(ctx, db, sourceTable)
	if err != nil {
		return "", err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Mode == "conditional" && (e.Status == statusActive || e.Status == statusPurged) {
			return e.HistoryTable, nil
		}
	}
	return "", fmt.Errorf("源表 %s 没有可核对的历史表（状态 active/purged）", sourceTable)
}

// runConditionalVerifyOnly 只做核对：读取迁移前落盘的基线，与历史表逐项比对，不迁移也不删除。
func runConditionalVerifyOnly(ctx context.Context, db *sql.DB, tc *TableConfig, cfg *MigrateConfig, rep *TableReport) *TableReport {
	rep.Stage = "verify"
	historyTable, err := latestArchivedHistoryTable(ctx, db, tc.Name)
	if err != nil {
		rep.Err = err
		return rep
	}
	base, notice, err := loadBaselineForVerify(ctx, db, cfg.Checkpoint.Dir, tc, historyTable)
	if err != nil {
		rep.Err = fmt.Errorf("无法使用基线文件复核（基线在复制阶段生成，请先执行一次复制）: %w", err)
		return rep
	}
	rep.HistoryTbl = historyTable
	rep.addWithRule("基线来源", true, notice,
		"文件基线，非实时采集；结构指纹已与历史表比对一致")
	if err := verifyAgainst(ctx, db, tc, historyTable, base, rep, ""); err != nil {
		rep.Err = err
	}
	return rep
}

// purgeCountVerdict 判断清理前的记录数校验是否通过，返回是否通过及原因。
//
// 这是修复“断点续跑与二次校验互相矛盾”的关键：续跑时源表已经少了一部分记录，
// 若仍要求“源表剩余数 == 历史表数”，续跑永远失败。正确的关系是
//
//	源表剩余待删数 + 已删除数 == 历史表记录数（且不得超过）。
//
// 只有在“单批次且从未删过”时，才要求严格相等，用于发现复制后新写入的业务数据。
func purgeCountVerdict(srcCount, histTotal, deletedSoFar int64, singleBatch, resuming bool) (bool, string) {
	if srcCount+deletedSoFar > histTotal {
		return false, "源表待删数与已删数之和超过历史表记录数，存在未归档数据"
	}
	if singleBatch && !resuming && srcCount != histTotal {
		return false, "单批次且未续跑时，源表待删数应等于历史表记录数（复制后可能有新数据写入）"
	}
	return true, ""
}

// runConditionalPurge 清理阶段：先二次校验，再分批删除源表。
func runConditionalPurge(ctx context.Context, db *sql.DB, tc *TableConfig, cfg *MigrateConfig) *TableReport {
	rep := &TableReport{Table: tc.Name, Mode: tc.Mode, Stage: "purge", OutputDir: cfg.Output.Dir}

	entries, err := activeConditionalHistoryTables(ctx, db, tc.Name)
	if err != nil {
		rep.Err = err
		return rep
	}
	if len(entries) == 0 {
		rep.Err = fmt.Errorf("源表 %s 没有状态为 active 的历史表：可能尚未执行复制阶段，或已清理完毕（已清理的批次会转为 purged 并进入保留期管理）", tc.Name)
		return rep
	}

	var tables []string
	var histTotal int64
	for _, e := range entries {
		ok, err := tableExists(ctx, db, e.HistoryTable)
		if err != nil {
			rep.Err = err
			return rep
		}
		if !ok {
			rep.addWithRule("历史表存在性", false, e.HistoryTable+" 登记存在但实际不存在", "")
			rep.Err = fmt.Errorf("历史表 %s 登记为 active 但实际不存在（可能被人工删除），未执行任何删除", e.HistoryTable)
			return rep
		}
		n, err := countRows(ctx, db, e.HistoryTable, "")
		if err != nil {
			rep.Err = err
			return rep
		}
		tables = append(tables, e.HistoryTable)
		histTotal += n
	}
	rep.HistoryTbl = strings.Join(tables, ",")

	// 先读进度，再做二次校验。续跑时源表已经少了一部分记录，
	// 若仍用“源表剩余数 == 历史表数”判等，续跑必然失败，断点续跑形同虚设。
	cp, err := loadCheckpoint(cfg.Checkpoint.Dir, tc.Name, StagePurge)
	if err != nil {
		rep.Err = err
		return rep
	}
	resuming := cp.validFor(tc, tables[0])
	deletedSoFar := int64(0)
	if resuming {
		deletedSoFar = cp.ProcessedRows
	}
	singleBatch := len(tables) == 1

	srcCount, err := countRows(ctx, db, tc.Name, tc.Condition)
	if err != nil {
		rep.Err = err
		return rep
	}

	// 校验规则：
	//   1) 源表剩余待删 + 已删 不得超过历史表总数——超出说明源表有记录未被归档，绝不删除；
	//   2) 单批次且未续跑时要求严格相等——用于发现“复制完成后、清理开始前”业务新写入的数据。
	countOK, countReason := purgeCountVerdict(srcCount, histTotal, deletedSoFar, len(tables) == 1, resuming)
	if !countOK {
		countReason = "；" + countReason
	}
	rep.addWithRule("清理前记录数校验", countOK,
		fmt.Sprintf("源表待删 %d / 历史表 %d / 已删 %d%s", srcCount, histTotal, deletedSoFar, countReason),
		"待删数+已删数不得超过历史表数；单批次未续跑时须严格相等")

	// 求和校验只在“单批次且未续跑”时做：多批次或续跑后源表已不完整，逐字段求和不可直接对比。
	if countOK && singleBatch && !resuming && len(tc.Verify.NumericSumFields) > 0 {
		kinds, err := columnKindMapFromDB(ctx, db, tables[0])
		if err != nil {
			rep.Err = err
			return rep
		}
		numCfg := NumericCompareConfig{AbsTol: tc.Verify.AbsTolerance, RelTol: tc.Verify.RelTolerance}
		srcSum, err := sumFields(ctx, db, tc.Name, tc.Condition, tc.Verify.NumericSumFields, kinds)
		if err != nil {
			rep.Err = err
			return rep
		}
		histSum, err := sumFields(ctx, db, tables[0], "", tc.Verify.NumericSumFields, kinds)
		if err != nil {
			rep.Err = err
			return rep
		}
		for _, f := range tc.Verify.NumericSumFields {
			pass, detail := srcSum[f].Compare(histSum[f], numCfg)
			rep.addWithRule("清理前SUM("+f+")", pass, detail, "删除前源表求和须等于历史表求和")
		}
	}

	if !rep.Pass() {
		rep.Err = fmt.Errorf("清理前校验未通过，已中止删除，源表未做任何改动")
		return rep
	}

	// 源表已无待删记录：不删除任何数据，仅补记状态。
	// 否则重复执行清理会因“源表 0 行 / 历史表 N 行”判等失败，历史表永远停在 active。
	if srcCount == 0 {
		rep.addWithRule("无需删除", true, "源表已无满足条件的记录", "")
		if err := markHistoryPurged(ctx, db, tables); err != nil {
			rep.addWithRule("更新历史表状态", false, err.Error(), "标记为 purged，进入保留期管理")
		} else {
			rep.addWithRule("更新历史表状态", true, strings.Join(tables, ","), "标记为 purged，进入保留期管理")
		}
		_ = removeCheckpoint(cfg.Checkpoint.Dir, tc.Name, StagePurge)
		return rep
	}

	lastKey := int64(0)
	if resuming {
		lastKey = cp.LastKey
		rep.addWithRule("断点续跑", true,
			fmt.Sprintf("从主键 %d 继续，已删除 %d 行", lastKey, deletedSoFar), "进度指纹与历史表一致")
	} else {
		cp = &Checkpoint{
			Table: tc.Name, Stage: string(StagePurge), Fingerprint: fingerprint(tc),
			HistoryTable: tables[0], LastKey: 0, ProcessedRows: 0,
			StartedAt: time.Now().Format(time.RFC3339), UpdatedAt: time.Now().Format(time.RFC3339),
		}
	}

	var minKey sql.NullInt64
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT MIN(%s) FROM %s WHERE (%s)", qIdent(tc.KeyColumn), qIdent(tc.Name), tc.Condition)).Scan(&minKey); err != nil {
		rep.Err = err
		return rep
	}
	if minKey.Valid && lastKey < minKey.Int64-1 {
		lastKey = minKey.Int64 - 1
	}
	if err := purgeBatches(ctx, db, tc, cfg, tables, lastKey, deletedSoFar, cp); err != nil {
		rep.Err = err
		return rep
	}

	remain, err := countRows(ctx, db, tc.Name, tc.Condition)
	if err != nil {
		rep.Err = err
		return rep
	}
	rep.addWithRule("清理后源表残留", remain == 0, fmt.Sprintf("残留 %d 行", remain), "源表满足条件的记录应全部删除")
	if remain == 0 {
		if err := markHistoryPurged(ctx, db, tables); err != nil {
			rep.addWithRule("更新历史表状态", false, err.Error(), "标记为 purged，进入保留期管理")
		} else {
			rep.addWithRule("更新历史表状态", true, strings.Join(tables, ","), "标记为 purged，进入保留期管理")
		}
		_ = removeCheckpoint(cfg.Checkpoint.Dir, tc.Name, StagePurge)
	}
	return rep
}

// purgeBatches 分批删除源表记录。
// 每批删除都带“该主键必须已存在于历史表”的条件，物理上不可能删掉未归档的数据。
func purgeBatches(ctx context.Context, db *sql.DB, tc *TableConfig, cfg *MigrateConfig, historyTables []string, lastKey, processed int64, cp *Checkpoint) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		sel := fmt.Sprintf("SELECT %s FROM %s WHERE %s > ? AND (%s) ORDER BY %s LIMIT %d",
			qIdent(tc.KeyColumn), qIdent(tc.Name), qIdent(tc.KeyColumn), tc.Condition, qIdent(tc.KeyColumn), tc.BatchSize)
		rows, err := tx.QueryContext(ctx, sel, lastKey)
		if err != nil {
			tx.Rollback()
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				tx.Rollback()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			tx.Rollback()
			return err
		}
		if len(ids) == 0 {
			tx.Rollback()
			break
		}

		ph := make([]string, len(ids))
		args := make([]interface{}, len(ids))
		for i, id := range ids {
			ph[i] = "?"
			args[i] = id
		}
		placeholders := strings.Join(ph, ",")

		// 删除条件：主键在本次批次内，且该主键确实存在于某一张历史表中。
		subs := make([]string, 0, len(historyTables))
		subArgs := make([]interface{}, 0, len(ids)*len(historyTables))
		for _, ht := range historyTables {
			subs = append(subs, fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)",
				qIdent(tc.KeyColumn), qIdent(ht), qIdent(tc.KeyColumn), placeholders))
			subArgs = append(subArgs, args...)
		}
		del := fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s) AND %s IN (SELECT %s FROM (%s) AS _h)",
			qIdent(tc.Name), qIdent(tc.KeyColumn), placeholders,
			qIdent(tc.KeyColumn), qIdent(tc.KeyColumn), strings.Join(subs, " UNION "))

		allArgs := append(append([]interface{}{}, args...), subArgs...)
		res, err := tx.ExecContext(ctx, del, allArgs...)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("删除源表批次失败: %w", err)
		}
		affected, _ := res.RowsAffected()
		if err := tx.Commit(); err != nil {
			return err
		}

		lastKey = ids[len(ids)-1]
		// 用实际删除行数累加，而不是本批次扫描到的行数：
		// “已删数”会参与续跑时的二次校验，必须真实，否则会放过未归档的数据。
		processed += affected
		cp.LastKey = lastKey
		cp.ProcessedRows = processed
		cp.UpdatedAt = time.Now().Format(time.RFC3339)
		if err := saveCheckpoint(cfg.Checkpoint.Dir, cp); err != nil {
			return err
		}
		fmt.Printf("  [%s] 已删除 %d 行（本批扫描 %d 行）\n", tc.Name, processed, len(ids))
	}
	return nil
}

func columnKindMapFromDB(ctx context.Context, db *sql.DB, table string) (map[string]NumericKind, error) {
	cols, err := tableColumns(ctx, db, table)
	if err != nil {
		return nil, err
	}
	return columnKindMap(cols), nil
}

// ---------------- CLI ----------------

func parseOptions() Options {
	configPath := flag.String("config", "migration.yaml", "配置文件路径")
	dryRun := flag.Bool("dry-run", false, "只采集基线与抽样，不执行迁移")
	purge := flag.Bool("purge", false, "执行清理阶段（删除源表中已归档的记录），默认关闭")
	// 与 history 子命令的 purge（删除到期历史表）重名时极易误操作，提供语义更明确的别名。
	purgeSource := flag.Bool("purge-source", false, "同 -purge：清理源表中已归档的记录，默认关闭")
	verifyOnly := flag.Bool("verify-only", false, "仅复核：用迁移前落盘的基线核对历史表，不迁移也不删除")
	action := flag.String("action", "migrate", "动作: migrate | history")
	historyOp := flag.String("history-op", "list", "history 子操作: list | merge | drop(别名 purge)")
	source := flag.String("source", "", "history 子操作的源表名")
	confirm := flag.Bool("confirm", false, "二次确认（history purge 必填）")
	flag.Parse()
	return Options{
		ConfigPath: *configPath, DryRun: *dryRun, Purge: *purge || *purgeSource, VerifyOnly: *verifyOnly,
		Action: *action, HistoryOp: *historyOp, Source: *source, Confirm: *confirm,
	}
}

func DbMigrateTool() {
	opt := parseOptions()
	cfg, err := LoadConfig(opt.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(2)
	}
	if err := EnsureOutputDir(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "准备目录失败: %v\n", err)
		os.Exit(2)
	}
	db, err := OpenDB(cfg.Database)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接数据库失败: %v\n", err)
		os.Exit(2)
	}
	defer db.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 试运行与清理互斥：dry-run 承诺“不写任何数据”，purge 会删除源表记录。
	// 旧实现里 purge 优先、静默忽略 dry-run，容易让人误以为看的是演练结果。
	if opt.DryRun && opt.Purge {
		fmt.Fprintln(os.Stderr, "-dry-run 与 -purge 不能同时使用：dry-run 不执行任何写操作，purge 会删除源表记录")
		os.Exit(2)
	}
	if opt.VerifyOnly && opt.Purge {
		fmt.Fprintln(os.Stderr, "-verify-only 与 -purge 不能同时使用：一个是只读核对，一个会删除源表记录")
		os.Exit(2)
	}
	var strictNotice string
	// 只有真正会写入数据的场景才需要严格模式保护：
	// dry-run 只采集基线、verify-only 只读核对，都不写行，没必要因为 sql_mode 把它们拦下来。
	if opt.Action == "migrate" && !opt.DryRun && !opt.VerifyOnly {
		strictNotice, err = ensureStrictMode(ctx, db, cfg.Database.StrictMode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "启动校验失败: %v\n", err)
			os.Exit(2)
		}
		if strictNotice != "" {
			fmt.Fprintf(os.Stderr, "\n⚠ 严格模式告警：%s\n\n", strictNotice)
		}
	}

	switch opt.Action {
	case "history":
		runHistoryAction(ctx, db, cfg, opt)
	case "migrate":
		runMigrateAction(ctx, db, cfg, opt, strictNotice)
	default:
		fmt.Fprintf(os.Stderr, "未知 action: %s\n", opt.Action)
		os.Exit(2)
	}
}

func runMigrateAction(ctx context.Context, db *sql.DB, cfg *MigrateConfig, opt Options, strictNotice string) {
	start := time.Now()
	allPass := true
	stamp := time.Now().Format("20060102_150405")
	for i := range cfg.Tables {
		tc := &cfg.Tables[i]
		fmt.Printf("\n>>> 开始处理表: %s (mode=%s)\n", tc.Name, tc.Mode)
		var rep *TableReport
		switch tc.Mode {
		case "full_table":
			rep = runFullTable(ctx, db, tc, cfg, opt)
		case "conditional":
			if opt.Purge {
				rep = runConditionalPurge(ctx, db, tc, cfg)
			} else {
				rep = runConditionalCopy(ctx, db, tc, cfg, opt, stamp)
			}
		}
		if strictNotice != "" && rep != nil {
			// 留痕：本次运行在无严格模式保护的前提下完成，写进报告与结果文件，
			// 而不是只在控制台一闪而过——事后追查要能看出当时的运行前提。
			rep.addWithRule("严格模式", true, strictNotice, "database.strict_mode="+cfg.Database.StrictMode)
		}
		rep.Print()
		if files, err := rep.WriteDetailFiles(); err != nil {
			fmt.Printf("  结果文件写入失败: %v\n", err)
		} else if len(files) > 0 {
			fmt.Printf("  结果文件已写入: %s\n", strings.Join(files, ", "))
		}
		if !rep.Pass() {
			allPass = false
		}
	}
	fmt.Printf("\n总耗时: %s\n", time.Since(start).Round(time.Millisecond))
	if !allPass {
		os.Exit(1)
	}
}

func runHistoryAction(ctx context.Context, db *sql.DB, cfg *MigrateConfig, opt Options) {
	switch opt.HistoryOp {
	case "list":
		source := opt.Source
		entries, err := listHistory(ctx, db, source)
		if err != nil {
			fmt.Fprintf(os.Stderr, "列示失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%-8s %-20s %-30s %-12s %-12s %-10s\n", "ID", "源表", "历史表", "模式", "记录数", "状态")
		for _, e := range entries {
			fmt.Printf("%-8d %-20s %-30s %-12s %-12d %-10s\n", e.ID, e.SourceTable, e.HistoryTable, e.Mode, e.RowCount, e.Status)
		}
	case "merge":
		source := opt.Source
		if source == "" {
			fmt.Fprintln(os.Stderr, "merge 需要 --source 指定源表名")
			os.Exit(2)
		}
		res, err := mergeHistory(ctx, db, source, cfg.Output.Dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "合并失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("合并完成: 批次=%d 归档表=%s 合并前=%d 期望=%d 合并后=%d 通过=%v\n",
			res.Batches, res.ArchiveTable, res.Before, res.Expected, res.After, res.Passed)
		if res.SkippedActive > 0 {
			fmt.Printf("另有 %d 个批次尚未清理源表（状态 active），已跳过；请先执行 -purge-source 再合并\n", res.SkippedActive)
		}
		if res.Batches == 0 && res.SkippedActive == 0 {
			fmt.Println("没有可合并的批次（仅合并源表已清理的批次）")
		}
		if path, err := WriteMergeResult(cfg.Output.Dir, res); err == nil {
			fmt.Printf("合并清单: %s\n", path)
		}
	case "purge", "drop":
		source := opt.Source
		if source == "" {
			fmt.Fprintln(os.Stderr, "purge 需要 --source 指定源表名")
			os.Exit(2)
		}
		res, err := purgeHistory(ctx, db, source, cfg.Output.Dir, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "清理扫描失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("到期历史表 %d 张:\n", len(res.Candidates))
		for _, c := range res.Candidates {
			fmt.Printf("  %s 记录数=%d 业务日期=%s 到期日=%s\n", c.HistoryTable, c.RowCount, c.BusinessDate, c.Expiry)
		}
		if path, err := WritePurgeList(cfg.Output.Dir, source, res.Candidates); err == nil {
			fmt.Printf("清理清单: %s\n", path)
		}
		if !opt.Confirm {
			fmt.Println("未指定 --confirm，仅列示候选，未执行删除。")
			return
		}
		if err := executePurge(ctx, db, source, res.Candidates, cfg.Output.Dir); err != nil {
			fmt.Fprintf(os.Stderr, "删除失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("清理完成。")
	default:
		fmt.Fprintf(os.Stderr, "未知 history-op: %s\n", opt.HistoryOp)
		os.Exit(2)
	}
}
