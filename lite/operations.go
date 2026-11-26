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

// BuildCopyQuery 生成一批搬运 SQL：按主键游标取一批，写入归档表，重复主键覆盖（幂等）。
func BuildCopyQuery(source, archive string, columns []string, keyCol, condition string, lastKey, batchSize int64) string {
	where := fmt.Sprintf("%s > %d", qIdent(keyCol), lastKey)
	if condition != "" {
		where = "(" + condition + ") AND " + where
	}
	colList := strings.Join(columns, ",")
	placeholders := make([]string, len(columns))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	_ = placeholders // 列值来自 SELECT，不用占位符
	selectSQL := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s LIMIT %d",
		colList, qIdent(source), where, qIdent(keyCol), batchSize)
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
		query := BuildCopyQuery(t.Name, t.ArchiveTable, columns, keyCol, t.Condition, lastKey, int64(t.BatchSize))
		res, err := db.ExecContext(ctx, query)
		if err != nil {
			return fmt.Errorf("搬运批次失败（lastKey=%d）: %w", lastKey, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取影响行数失败: %w", err)
		}
		if affected == 0 {
			break // 没有更多数据
		}
		rowsDone += affected

		// 取本批最大主键，推进游标。
		maxSQL := fmt.Sprintf("SELECT MAX(%s) FROM (SELECT %s FROM %s WHERE %s > %d ORDER BY %s LIMIT %d) AS batch",
			qIdent(keyCol), qIdent(keyCol), qIdent(t.Name), qIdent(keyCol), lastKey, qIdent(keyCol), t.BatchSize)
		var nextKey sql.NullInt64
		if err := db.QueryRowContext(ctx, maxSQL).Scan(&nextKey); err != nil {
			return fmt.Errorf("读取批次主键上界失败: %w", err)
		}
		if !nextKey.Valid || nextKey.Int64 <= lastKey {
			return fmt.Errorf("游标未前进（lastKey=%d，next=%v），为避免死循环已停止", lastKey, nextKey)
		}
		lastKey = nextKey.Int64

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
