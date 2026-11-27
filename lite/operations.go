// 核心操作：条件模式的搬运与按主键删除、全表模式的建空表与原子改名。
//
// 安全逻辑拆成两层：
//   - 纯决策函数（JudgeXxx）：只吃参数、只做判断，便于单测覆盖所有分支。
//   - 执行函数（RunXxx）：查库、执行 SQL、落进度。
//
// 删除路径的红线：只删"已校验通过的主键"，删前做守恒核对，删后命中数必须为 0。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ============ 条件模式：分批搬运 ============

// BuildCopySelectQuery 生成"本批待搬运数据"的 SELECT。
// 搬运写入和游标推进必须共用本函数，否则两处 WHERE 一旦漂移，就会按全表主键推进、
// 却只搬命中条件的数据，导致游标越过尚未搬运的行而漏数据。
func BuildCopySelectQuery(source string, columns []string, keyCol, condition string, lastKey, batchSize int64) string {
	where := fmt.Sprintf("%s > %d", qIdent(keyCol), lastKey)
	if condition != "" {
		where = "(" + condition + ") AND " + where
	}
	colList := strings.Join(columns, ",")
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT %d",
		colList, qIdent(source), where, qIdent(keyCol), batchSize)
}

// BuildCopyQuery 生成一批搬运 SQL：按主键游标取一批，写入归档表，重复主键覆盖（幂等）。
func BuildCopyQuery(source, archive string, columns []string, keyCol, condition string, lastKey, batchSize int64) string {
	colList := strings.Join(columns, ",")
	selectSQL := BuildCopySelectQuery(source, columns, keyCol, condition, lastKey, batchSize)
	updates := make([]string, 0, len(columns))
	for _, c := range columns {
		updates = append(updates, fmt.Sprintf("%s = VALUES(%s)", c, c))
	}
	return fmt.Sprintf("INSERT INTO %s (%s) %s ON DUPLICATE KEY UPDATE %s",
		qIdent(archive), colList, selectSQL, strings.Join(updates, ","))
}

// BuildArchiveCountQuery 生成归档表行数统计（条件模式归档表只装命中数据，全表即全部）。
func BuildArchiveCountQuery(archive, condition string) string {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s", qIdent(archive))
	if condition != "" {
		q += " WHERE " + condition
	}
	return q
}

// RunCopy 执行条件模式的分批搬运，按批落进度。
func RunCopy(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg, keyCol string, columns []string, cp *Checkpoint) error {
	var lastKey int64
	var rowsDone int64
	if cp != nil {
		lastKey = cp.LastKey
		rowsDone = cp.RowsDone
	}
	for {
		// 先读出本批命中条件的主键。列表为空才算真的没有更多数据。
		// 不能用 INSERT ... ON DUPLICATE KEY UPDATE 的 RowsAffected 判断结束：
		// 行已存在且值未变时它也返回 0，断点续跑会被误判成"没有更多数据"。
		keysSQL := BuildCopySelectQuery(t.Name, []string{qIdent(keyCol)}, keyCol, t.Condition, lastKey, int64(t.BatchSize))
		rows, err := db.QueryContext(ctx, keysSQL)
		if err != nil {
			return fmt.Errorf("读取搬运批次失败（lastKey=%d）: %w", lastKey, err)
		}
		var keys []int64
		for rows.Next() {
			var k int64
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return fmt.Errorf("读取搬运批次主键失败（lastKey=%d）: %w", lastKey, err)
			}
			keys = append(keys, k)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		state, err := JudgeCopyBatch(keys, lastKey, rowsDone)
		if err != nil {
			return err
		}
		if state.Done {
			break
		}

		query := BuildCopyQuery(t.Name, t.ArchiveTable, columns, keyCol, t.Condition, lastKey, int64(t.BatchSize))
		if _, err := db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("搬运批次失败（lastKey=%d）: %w", lastKey, err)
		}
		lastKey = state.LastKey
		rowsDone = state.RowsDone

		newCP := &Checkpoint{
			Fingerprint: ConfigFingerprint(*t),
			Table:       t.Name,
			LastKey:     lastKey,
			RowsDone:    rowsDone,
			Stage:       "copied",
		}
		if err := SaveCheckpoint(cfg.Checkpoint.Dir, t.Name, newCP); err != nil {
			return err
		}
	}
	return nil
}

// CopyBatchState 是本批搬运后应更新的进度。
type CopyBatchState struct {
	LastKey  int64
	RowsDone int64
	Done     bool // 本批为空，没有更多符合条件的数据
}

// JudgeCopyBatch 根据本批查到的主键列表决定游标如何推进。
// keys 按主键升序排列，最后一条即本批上界；列表为空才表示真的搬完了。
func JudgeCopyBatch(keys []int64, lastKey, rowsDone int64) (CopyBatchState, error) {
	if len(keys) == 0 {
		return CopyBatchState{LastKey: lastKey, RowsDone: rowsDone, Done: true}, nil
	}
	next := keys[len(keys)-1]
	if next <= lastKey {
		return CopyBatchState{}, fmt.Errorf("游标未前进（lastKey=%d，next=%d），为避免死循环已停止", lastKey, next)
	}
	return CopyBatchState{LastKey: next, RowsDone: rowsDone + int64(len(keys))}, nil
}

// JudgeCopyCounts 判定搬运结束后的行数是否对齐。
// 搬运循环退出只说明游标走到了头，不等于命中条件的数据都搬完了；
// 只有两侧行数相等才能对外报"搬运完成"。
func JudgeCopyCounts(srcCount, archiveCount int64) error {
	if srcCount == archiveCount {
		return nil
	}
	diff := srcCount - archiveCount
	if diff < 0 {
		diff = -diff
	}
	return fmt.Errorf("搬运结束但行数不一致：源表命中 %d 行，归档表 %d 行（相差 %d 行）；请勿执行 purge，先重新搬运并核对", srcCount, archiveCount, diff)
}

// ============ 条件模式：按主键清单删除 ============

// PurgeVerdict 是删除前的守恒判定输入。
type PurgeVerdict struct {
	SrcRowsToDelete int64 // 源表当前待删行数（满足条件的行数）
	DeletedSoFar    int64 // 本次任务已删行数（续跑场景）
	ArchiveRows     int64 // 归档表对应行数
	BatchRows       int64 // 本批打算删除的主键个数
	IsFinalBatch    bool  // 是否最后一批
}

// JudgePurgeAllowed 判断本批删除是否允许。
// 守恒式：待删 + 已删 == 归档行数，且不得超出。
// 最后一批还要求归档侧不能再多，确保没有漏搬或多搬。
func JudgePurgeAllowed(v PurgeVerdict) error {
	if v.SrcRowsToDelete < 0 || v.DeletedSoFar < 0 || v.ArchiveRows < 0 || v.BatchRows < 0 {
		return fmt.Errorf("计数出现负数，数据异常")
	}
	if v.BatchRows == 0 {
		return fmt.Errorf("本批删除主键数为 0，拒绝执行")
	}
	total := v.SrcRowsToDelete + v.DeletedSoFar
	if total > v.ArchiveRows {
		return fmt.Errorf("源表待删(%d)+已删(%d)=%d 超过归档表行数(%d)，说明归档数据不足，拒绝删除",
			v.SrcRowsToDelete, v.DeletedSoFar, total, v.ArchiveRows)
	}
	if total < v.ArchiveRows {
		return fmt.Errorf("源表待删(%d)+已删(%d)=%d 小于归档表行数(%d)，两侧未对齐，拒绝删除",
			v.SrcRowsToDelete, v.DeletedSoFar, total, v.ArchiveRows)
	}
	if v.BatchRows > v.SrcRowsToDelete {
		return fmt.Errorf("本批要删(%d)超过源表待删(%d)，拒绝删除", v.BatchRows, v.SrcRowsToDelete)
	}
	return nil
}

// BuildDeleteByKeys 生成按主键清单删除的 SQL（绝不用原条件直接删）。
func BuildDeleteByKeys(table, keyCol string, keys []int64) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d", k))
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s IN (%s)", qIdent(table), qIdent(keyCol), strings.Join(parts, ","))
}

// BuildRemainingCountQuery 生成删除后源表残留命中数的检查 SQL，期望结果为 0。
func BuildRemainingCountQuery(table, keyCol, condition string, maxArchivedKey int64) string {
	where := "(" + condition + ")"
	if keyCol != "" {
		where += fmt.Sprintf(" AND %s <= %d", qIdent(keyCol), maxArchivedKey)
	}
	return fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", qIdent(table), where)
}

// ApplyPurgeBatch 按本批实际删除行数推进计数。
// 待删数递减、已删数累加，两者之和保持不变，守恒判定因此不需要重新查库。
func ApplyPurgeBatch(srcRemaining, deletedSoFar, affected int64) (int64, int64) {
	return srcRemaining - affected, deletedSoFar + affected
}

// BuildNewWriteCheckQuery 生成"是否有新数据落入归档范围"的检查 SQL。
// 只看主键大于已归档最大主键、且满足迁移条件的行；命中一条即返回，走主键索引。
// 删除开始前和结束后各跑一次，用于补回增量计数丢失的"有人在写数据"提醒。
func BuildNewWriteCheckQuery(table, condition, keyCol string, maxArchivedKey int64) string {
	clause := fmt.Sprintf("%s > %d", qIdent(keyCol), maxArchivedKey)
	if condition != "" {
		clause = "(" + condition + ") AND " + clause
	}
	return fmt.Sprintf("SELECT 1 FROM %s WHERE %s LIMIT 1", qIdent(table), clause)
}

// BuildArchivedKeysQuery 生成从归档表取待删主键清单的 SQL。
// startKey 是续跑起点：只取主键大于它的行，避免重复删除。
func BuildArchivedKeysQuery(archive, keyCol string, startKey int64, batchSize int) string {
	return fmt.Sprintf("SELECT %s FROM %s WHERE %s > %d ORDER BY %s LIMIT %d",
		qIdent(keyCol), qIdent(archive), qIdent(keyCol), startKey, qIdent(keyCol), batchSize)
}

// ============ 全表模式：预检 + 建空表 + 自增值对齐 + 原子改名 ============

// BuildCreateEmptyTable 生成按源表结构建空表的 SQL。
func BuildCreateEmptyTable(source, empty string) string {
	return fmt.Sprintf("CREATE TABLE %s LIKE %s", qIdent(empty), qIdent(source))
}

// BuildAlignAutoIncrement 生成把空表自增值对齐到源表最大值的 SQL。
func BuildAlignAutoIncrement(empty, keyCol string, maxKey int64) string {
	return fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", qIdent(empty), maxKey+1)
}

// BuildRenameTable 生成原子改名 SQL：源表→归档表，空表→源表名。
func BuildRenameTable(source, archive, empty string) string {
	return fmt.Sprintf("RENAME TABLE %s TO %s, %s TO %s",
		qIdent(source), qIdent(archive), qIdent(empty), qIdent(source))
}

// JudgeMaxKeyForAutoIncrement 校验自增值对齐的输入。
func JudgeMaxKeyForAutoIncrement(maxKey int64) error {
	if maxKey < 0 {
		return fmt.Errorf("主键最大值不能为负数: %d", maxKey)
	}
	return nil
}

// RunFullTable 执行全表模式的预检与原子切换。
func RunFullTable(ctx context.Context, db *sql.DB, t *TableCfg) error {
	// 预检一：归档表不能已存在。
	exists, err := tableExists(ctx, db, t.ArchiveTable)
	if err != nil {
		return fmt.Errorf("检查归档表失败: %w", err)
	}
	if err := judgeArchiveTableFree(exists, t.ArchiveTable); err != nil {
		return err
	}

	// 预检二：不能有外键或视图依赖。
	deps, err := probeDependencies(ctx, db, t.Name)
	if err != nil {
		return fmt.Errorf("检查表依赖失败: %w", err)
	}
	if err := judgeDependenciesAllowed(deps); err != nil {
		return err
	}

	// 探测主键（全表模式也要整型主键，用于自增值对齐）。
	pk, err := probePrimaryKey(ctx, db, t.Name)
	if err != nil {
		return err
	}
	keyCol, err := judgePrimaryKeyUsable(t.KeyColumn, pk)
	if err != nil {
		return err
	}

	// 空表临时名，确保不与现有对象冲突。
	emptyTable := t.Name + "__new_empty"
	emptyExists, err := tableExists(ctx, db, emptyTable)
	if err != nil {
		return err
	}
	if emptyExists {
		return fmt.Errorf("临时空表 %s 已存在，请先清理残留", emptyTable)
	}

	// 自增值对齐。
	maxKey, err := maxKeyValue(ctx, db, t.Name, keyCol)
	if err != nil {
		return fmt.Errorf("读取主键最大值失败: %w", err)
	}
	if err := JudgeMaxKeyForAutoIncrement(maxKey); err != nil {
		return err
	}

	// 建空表 → 对齐自增值 → 原子改名。
	stmts := []string{
		BuildCreateEmptyTable(t.Name, emptyTable),
		BuildAlignAutoIncrement(emptyTable, keyCol, maxKey),
		BuildRenameTable(t.Name, t.ArchiveTable, emptyTable),
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("执行 %q 失败: %w", q, err)
		}
	}
	return nil
}
