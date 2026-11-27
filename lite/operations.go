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
	"strconv"
	"strings"
	"time"
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

// ProgressPercent 计算搬运完成百分比，截断到 0~100。
// total<=0 表示没有命中数据，直接算作完成，避免除零。
func ProgressPercent(done, total int64) int {
	if total <= 0 {
		return 100
	}
	if done <= 0 {
		return 0
	}
	p := int(done * 100 / total)
	if p > 100 {
		return 100
	}
	return p
}

// FormatProgressBar 生成固定宽度的文本进度条，例如 "[====>     ]"。
// 全部完成时全是等号，不再显示游标。
func FormatProgressBar(percent, width int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	if width < 2 {
		width = 2
	}
	filled := percent * width / 100
	if filled > width {
		filled = width
	}
	var sb strings.Builder
	sb.WriteByte('[')
	sb.WriteString(strings.Repeat("=", filled))
	if filled < width {
		sb.WriteByte('>')
		sb.WriteString(strings.Repeat(" ", width-filled-1))
	}
	sb.WriteByte(']')
	return sb.String()
}

// FormatCount 给整数加千分位逗号，便于阅读大数字。
func FormatCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var sb strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			sb.WriteByte(',')
		}
		sb.WriteRune(c)
	}
	if neg {
		return "-" + sb.String()
	}
	return sb.String()
}

// FormatDuration 把耗时格式化为 00:32 或 1:02:03。
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int64(d / time.Second)
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// BuildProgressLine 拼出一行完整进度提示。
func BuildProgressLine(done, total int64, percent int, elapsed time.Duration) string {
	return fmt.Sprintf("搬运进度: %s %3d%%  %s / %s  用时 %s",
		FormatProgressBar(percent, 24), percent, FormatCount(done), FormatCount(total), FormatDuration(elapsed))
}

// RunCopy 执行条件模式的分批搬运，按批落进度。
func RunCopy(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg, keyCol string, columns []string, cp *Checkpoint, total int64) error {
	var lastKey int64
	var rowsDone int64
	if cp != nil {
		lastKey = cp.LastKey
		rowsDone = cp.RowsDone
	}
	started := time.Now()
	lastPercent := ProgressPercent(rowsDone, total) - 1 // 保证首批必定刷新一次
	// 续跑时先把上次累计的进度显示出来，用户能立刻看到从哪接上。
	fmt.Print("\r" + BuildProgressLine(rowsDone, total, lastPercent+1, 0))
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
		// 只在百分比整数变化时刷新，避免每批都刷屏；日志重定向时最多约 100 行。
		if pct := ProgressPercent(rowsDone, total); pct != lastPercent {
			fmt.Print("\r" + BuildProgressLine(rowsDone, total, pct, time.Since(started)))
			lastPercent = pct
		}
	}
	// 确保最终进度是 100%，并换行，让后续的核对结果另起一行。
	fmt.Print("\r" + BuildProgressLine(rowsDone, total, ProgressPercent(rowsDone, total), time.Since(started)) + "\n")
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
}

// JudgePurgeAllowed 判断本批删除是否允许。
// 守恒式：待删 + 已删 必须严格等于归档行数——多出来说明归档数据不足，
// 少了说明还有没搬完的行，两个方向都不允许删。
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

// AlignAutoIncrementValue 计算新表应设置的自增值：取"主键最大值 + 1"与"源表当前自增值"中的较大者。
// 只按 MAX+1 对齐，在源表删过历史行时会倒退，让新表重新发放已经用过的 id。
func AlignAutoIncrementValue(maxKey, currentAutoInc int64) int64 {
	next := maxKey + 1
	if currentAutoInc > next {
		next = currentAutoInc
	}
	if next < 1 {
		next = 1
	}
	return next
}

// BuildAlignAutoIncrement 生成把空表自增值设为 next 的 SQL。
func BuildAlignAutoIncrement(empty, keyCol string, next int64) string {
	return fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", qIdent(empty), next)
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
// force 只影响"改名会让新表丢掉触发器/外键"这一类可接受副作用，其余预检照旧拒绝。
func RunFullTable(ctx context.Context, db *sql.DB, t *TableCfg, force bool) error {
	// 预检一：归档表不能已存在。
	exists, err := tableExists(ctx, db, t.ArchiveTable)
	if err != nil {
		return fmt.Errorf("检查归档表失败: %w", err)
	}
	if err := judgeArchiveTableFree(exists, t.ArchiveTable); err != nil {
		return err
	}

	// 预检二：别人不能引用这张表（入向外键、视图），否则改名会牵连别的对象。
	deps, err := probeDependencies(ctx, db, t.Name)
	if err != nil {
		return fmt.Errorf("检查表依赖失败: %w", err)
	}
	if err := judgeDependenciesAllowed(deps); err != nil {
		return err
	}

	// 自增值对齐只对整型单列主键可行；探测提前到这里，因为"哪一列会被对齐"
	// 决定了改名后还有没有自增列会从 1 重新发号。
	keyCol, skipReason, err := chooseAutoIncrementKey(ctx, db, t.Name, t.KeyColumn)
	if err != nil {
		return err
	}

	// 预检三：本表自己的触发器、外键、非主键自增列都不会被 CREATE TABLE ... LIKE 复制，
	// 也不会随改名保留计数器。这类副作用不破坏被归档的数据，但会让业务侧静默失去
	// 审计/同步/约束，或重发已用过的编号，默认拒绝，-force 放行。
	triggers, err := probeTableTriggers(ctx, db, t.Name)
	if err != nil {
		return fmt.Errorf("检查触发器失败: %w", err)
	}
	outFKs, err := probeOutboundForeignKeys(ctx, db, t.Name)
	if err != nil {
		return fmt.Errorf("检查外键失败: %w", err)
	}
	autoIncCols, err := probeAutoIncrementColumns(ctx, db, t.Name)
	if err != nil {
		return fmt.Errorf("检查自增列失败: %w", err)
	}
	if sideErr := judgeRenameSideEffects(triggers, outFKs, autoIncCols, keyCol); sideErr != nil {
		if !force {
			return sideErr
		}
		fmt.Printf("警告: %v\n已按 -force 继续。请自行在新表上重建触发器与外键，并把其余自增列的计数器调到安全值。\n", sideErr)
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

	// 建空表 →（可选）对齐自增值 → 原子改名。
	stmts := []string{BuildCreateEmptyTable(t.Name, emptyTable)}
	if keyCol == "" {
		fmt.Printf("表 %s 跳过自增值对齐：%s；改名本身不受影响。\n", t.Name, skipReason)
	} else {
		maxKey, err := maxKeyValue(ctx, db, t.Name, keyCol)
		if err != nil {
			return fmt.Errorf("读取主键最大值失败: %w", err)
		}
		if err := JudgeMaxKeyForAutoIncrement(maxKey); err != nil {
			return err
		}
		currentAutoInc, err := sourceAutoIncrement(ctx, db, t.Name)
		if err != nil {
			return fmt.Errorf("读取源表自增值失败: %w", err)
		}
		next := AlignAutoIncrementValue(maxKey, currentAutoInc)
		fmt.Printf("表 %s 新表自增值将设为 %d（主键最大值 %d，源表当前自增值 %d）。\n", t.Name, next, maxKey, currentAutoInc)
		stmts = append(stmts, BuildAlignAutoIncrement(emptyTable, keyCol, next))
	}
	stmts = append(stmts, BuildRenameTable(t.Name, t.ArchiveTable, emptyTable))

	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("执行 %q 失败: %w", q, err)
		}
	}
	return nil
}
