package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const registryTable = "_dbmigration_history_registry"

// 历史表登记状态。区分“源表记录是否已清理”和“历史表本身是否还在”，
// 这两个生命周期阶段不同：源表清理后历史表仍要按保留期保留。
const (
	statusActive  = "active"  // 已复制，源表记录尚未清理
	statusPurged  = "purged"  // 源表记录已清理，历史表仍保留待到期
	statusMerged  = "merged"  // 已合并进总归档表
	statusDropped = "dropped" // 历史表本身已删除
)

// ensureRegistry 创建历史表登记表（幂等）。
func ensureRegistry(ctx context.Context, db *sql.DB) error {
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		source_table VARCHAR(128) NOT NULL,
		history_table VARCHAR(128) NOT NULL,
		mode VARCHAR(16) NOT NULL,
		row_count BIGINT NOT NULL DEFAULT 0,
		source_condition TEXT,
		retention_days INT NOT NULL DEFAULT 365,
		created_at DATETIME NOT NULL,
		business_date DATE NULL,
		status VARCHAR(16) NOT NULL DEFAULT 'active',
		UNIQUE KEY uk_history_table (history_table),
		INDEX idx_source_status (source_table, status)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`, qIdent(registryTable))
	_, err := db.ExecContext(ctx, ddl)
	return err
}

// registerHistory 登记一张历史表。
func registerHistory(ctx context.Context, db *sql.DB, tc *TableConfig, historyTable string, rowCount int64) error {
	if err := ensureRegistry(ctx, db); err != nil {
		return fmt.Errorf("初始化登记表失败: %w", err)
	}
	businessDate := time.Now().Format("2006-01-02")

	// 同一张历史表可能被重复登记：全表模式覆盖迁移、条件迁移断点续跑都会再走一次登记。
	// 若直接插入新行，登记表会出现重复项，清理阶段的记录数会被重复累加。
	var existing int64
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE history_table=?", qIdent(registryTable)), historyTable).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		_, err := db.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s SET row_count=?, source_condition=?, retention_days=?, created_at=?, business_date=?, status=?
			 WHERE history_table=?`, qIdent(registryTable)),
			rowCount, tc.Condition, tc.RetentionDays, time.Now(), businessDate, statusActive, historyTable)
		return err
	}

	insert := fmt.Sprintf(`INSERT INTO %s
		(source_table, history_table, mode, row_count, source_condition, retention_days, created_at, business_date, status)
		VALUES (?,?,?,?,?,?,?,?,?)`, qIdent(registryTable))
	_, err := db.ExecContext(ctx, insert, tc.Name, historyTable, tc.Mode, rowCount, tc.Condition, tc.RetentionDays, time.Now(), businessDate, statusActive)
	return err
}

// markHistoryPurged 把历史表登记状态标记为“源表记录已清理”。
// 源表记录清理成功后必须打这个标记，否则再次执行清理会因“源表 0 行 / 历史表 N 行”
// 判定失败，该历史表会永远停留在 active，无法进入保留期清理流程。
func markHistoryPurged(ctx context.Context, db *sql.DB, historyTables []string) error {
	if len(historyTables) == 0 {
		return nil
	}
	if err := ensureRegistry(ctx, db); err != nil {
		return err
	}
	for _, h := range historyTables {
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf("UPDATE %s SET status=? WHERE history_table=? AND status=?", qIdent(registryTable)),
			statusPurged, h, statusActive); err != nil {
			return fmt.Errorf("更新历史表 %s 状态失败: %w", h, err)
		}
	}
	return nil
}

// HistoryEntry 是登记表的一条记录。
type HistoryEntry struct {
	ID            int64
	SourceTable   string
	HistoryTable  string
	Mode          string
	RowCount      int64
	Condition     string
	RetentionDays int
	CreatedAt     string
	BusinessDate  string
	Status        string
}

// listHistory 列出历史表登记记录。sourceTable 为空表示列出全部。
func listHistory(ctx context.Context, db *sql.DB, sourceTable string) ([]HistoryEntry, error) {
	if err := ensureRegistry(ctx, db); err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT id, source_table, history_table, mode, row_count,
		IFNULL(source_condition,''), retention_days, created_at,
		IFNULL(business_date,''), status FROM %s`, qIdent(registryTable))
	var args []interface{}
	if sourceTable != "" {
		q += " WHERE source_table = ?"
		args = append(args, sourceTable)
	}
	q += " ORDER BY id"
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		var created string
		if err := rows.Scan(&e.ID, &e.SourceTable, &e.HistoryTable, &e.Mode, &e.RowCount,
			&e.Condition, &e.RetentionDays, &created, &e.BusinessDate, &e.Status); err != nil {
			return nil, err
		}
		e.CreatedAt = created
		out = append(out, e)
	}
	return out, rows.Err()
}

// countRowsTx 在事务内统计记录数。
// 合并流程必须在同一个事务连接上统计：用连接池另取连接会读不到未提交的插入数据，
// 导致“合并后条数”恒偏小、永远判定为不一致并回滚。
func countRowsTx(ctx context.Context, tx *sql.Tx, table string) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", qIdent(table))).Scan(&n)
	return n, err
}

// mergeHistory 把 sourceTable 下所有“源表已清理”（status=purged）的条件迁移批次历史表
// 合并到一张总归档表。合并前后比对总记录数；不一致则回滚并报错，不删除任何批次表。
//
// 只合并已清理批次，是为了保持生命周期顺序：先复制 → 再核对 → 再清理源表 → 最后合并归档。
// 若在源表未清理时就合并并删除批次表，后续清理阶段将找不到核对依据。
func mergeHistory(ctx context.Context, db *sql.DB, sourceTable string, outputDir string) (*MergeResult, error) {
	entries, err := listHistory(ctx, db, sourceTable)
	if err != nil {
		return nil, err
	}
	var batches []HistoryEntry
	skippedActive := 0
	for _, e := range entries {
		if e.Mode != "conditional" {
			continue
		}
		if e.Status == statusActive {
			skippedActive++
			continue
		}
		if e.Status != statusPurged {
			continue
		}
		ok, err := tableExists(ctx, db, e.HistoryTable)
		if err != nil {
			return nil, err
		}
		if !ok {
			// 登记了但表已不存在（人工删除等），跳过并继续，不让合并整体失败。
			continue
		}
		batches = append(batches, e)
	}
	res := &MergeResult{SourceTable: sourceTable, Batches: len(batches), SkippedActive: skippedActive}
	if len(batches) == 0 {
		return res, nil
	}

	archiveTable := sourceTable + "_archive_all"
	// 用第一张批次表的结构创建总归档表；若已存在则复用。
	exists, err := tableExists(ctx, db, archiveTable)
	if err != nil {
		return nil, err
	}
	if !exists {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s LIKE %s", qIdent(archiveTable), qIdent(batches[0].HistoryTable))); err != nil {
			return nil, fmt.Errorf("创建总归档表失败: %w", err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	archiveBefore, err := countRowsTx(ctx, tx, archiveTable)
	if err != nil {
		return nil, err
	}

	var expected int64
	for _, b := range batches {
		n, err := countRowsTx(ctx, tx, b.HistoryTable)
		if err != nil {
			return nil, err
		}
		expected += n
	}

	for _, b := range batches {
		cols, err := insertableColumns(ctx, db, b.HistoryTable)
		if err != nil {
			return nil, err
		}
		colList := strings.Join(cols, ", ")
		ins := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s",
			qIdent(archiveTable), colList, colList, qIdent(b.HistoryTable))
		if _, err := tx.ExecContext(ctx, ins); err != nil {
			return nil, fmt.Errorf("合并批次表 %s 失败: %w", b.HistoryTable, err)
		}
	}

	archiveAfter, err := countRowsTx(ctx, tx, archiveTable)
	if err != nil {
		return nil, err
	}
	res.Before = archiveBefore
	res.After = archiveAfter
	res.Expected = archiveBefore + expected
	if archiveAfter != res.Expected {
		return res, fmt.Errorf("合并记录数不一致: 合并后 %d, 期望 %d", archiveAfter, res.Expected)
	}

	// 更新登记表状态为 merged（在同一个事务内）。
	for _, b := range batches {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET status=? WHERE id=?", qIdent(registryTable)), statusMerged, b.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// 提交成功后删除已合并的批次空表，并写合并清单。
	var cleaned []string
	for _, b := range batches {
		if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+qIdent(b.HistoryTable)); err != nil {
			cleaned = append(cleaned, b.HistoryTable+"(删除失败:"+err.Error()+")")
		} else {
			cleaned = append(cleaned, b.HistoryTable)
		}
	}
	res.Cleaned = cleaned
	res.ArchiveTable = archiveTable
	res.Passed = true
	return res, nil
}

// MergeResult 是合并结果。
type MergeResult struct {
	SourceTable   string
	ArchiveTable  string
	Batches       int
	SkippedActive int // 因源表尚未清理而跳过的批次数
	Before        int64
	Expected      int64
	After         int64
	Cleaned       []string
	Passed        bool
}

// purgeHistory 删除超过保留期的历史表（仅 status=active 且 business_date 到期）。
// 需显式调用，删除前产出清单。
func purgeHistory(ctx context.Context, db *sql.DB, sourceTable string, outputDir string, now time.Time) (*PurgeResult, error) {
	entries, err := listHistory(ctx, db, sourceTable)
	if err != nil {
		return nil, err
	}
	res := &PurgeResult{SourceTable: sourceTable}
	for _, e := range entries {
		// active 与 purged 都进入保留期管理：前者源表还没清理，后者已清理但历史表仍在服务保留期。
		if (e.Status != statusActive && e.Status != statusPurged) || e.BusinessDate == "" {
			continue
		}
		bd, err := time.Parse("2006-01-02", e.BusinessDate)
		if err != nil {
			continue
		}
		expiry := bd.AddDate(0, 0, e.RetentionDays)
		if now.Before(expiry) {
			continue
		}
		res.Candidates = append(res.Candidates, PurgeCandidate{
			HistoryTable: e.HistoryTable, RowCount: e.RowCount, BusinessDate: e.BusinessDate,
			RetentionDays: e.RetentionDays, Expiry: expiry.Format("2006-01-02"),
		})
	}
	return res, nil
}

// PurgeCandidate 是待清理候选。
type PurgeCandidate struct {
	HistoryTable  string
	RowCount      int64
	BusinessDate  string
	RetentionDays int
	Expiry        string
}

// PurgeResult 是清理候选结果。
type PurgeResult struct {
	SourceTable string
	Candidates  []PurgeCandidate
}

// executePurge 实际删除候选历史表并更新登记表状态。
func executePurge(ctx context.Context, db *sql.DB, sourceTable string, candidates []PurgeCandidate, outputDir string) error {
	if len(candidates) == 0 {
		return nil
	}
	if err := ensureRegistry(ctx, db); err != nil {
		return err
	}
	for _, c := range candidates {
		if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+qIdent(c.HistoryTable)); err != nil {
			return fmt.Errorf("删除历史表 %s 失败: %w", c.HistoryTable, err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET status=? WHERE history_table=?", qIdent(registryTable)), statusDropped, c.HistoryTable); err != nil {
			return err
		}
	}
	return nil
}
