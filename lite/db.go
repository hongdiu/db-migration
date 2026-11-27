// 数据库连接、元数据探测与预检。
//
// 预检尽量做成"探测结果 + 纯决策函数"两段：探测函数只负责查库，
// 决策函数只负责判断，便于单元测试覆盖而不需要真实数据库。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func OpenDB(cfg DBConfig) (*sql.DB, error) {
	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return db, nil
}

// qIdent 返回带反引号的标识符。配置校验已保证标识符合法。
func qIdent(name string) string {
	if !identRe.MatchString(name) {
		panic(fmt.Sprintf("非法标识符: %q", name))
	}
	return "`" + name + "`"
}

// ColumnMeta 是单列元数据。
type ColumnMeta struct {
	Name     string
	DataType string
	// ColumnType 是带长度/精度的完整类型（如 varchar(255)、decimal(18,2)）。
	// 只比 DataType 会漏掉 varchar(10) 与 varchar(255) 这类差异，结构比对用这一列。
	ColumnType string
	Extra      string
}

// tableColumns 读取表的全部列（按定义顺序）。
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]ColumnMeta, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IFNULL(EXTRA,'')
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []ColumnMeta
	for rows.Next() {
		var c ColumnMeta
		if err := rows.Scan(&c.Name, &c.DataType, &c.ColumnType, &c.Extra); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("表 %s 不存在或无列信息", table)
	}
	return cols, nil
}

// CompareTableStructure 比对源表与归档表的列定义（列名、完整类型、顺序）。
// 条件模式允许复用已存在的归档表（为了中断续跑），但前提是它确实是源表的副本。
// 若列名相同而类型不同，写入时数据库会静默转换，校验又按同一套转换规则比对，
// 可能一路"通过"却已经改了值，所以在搬运前就要拦住。
func CompareTableStructure(srcTable, srcArchive string, src, dst []ColumnMeta) error {
	if len(src) != len(dst) {
		return fmt.Errorf("归档表 %s 与源表 %s 列数不一致：源表 %d 列，归档表 %d 列；请换用与源表结构一致的归档表",
			srcArchive, srcTable, len(src), len(dst))
	}
	for i := range src {
		if !strings.EqualFold(src[i].Name, dst[i].Name) {
			return fmt.Errorf("第 %d 列列名不一致：源表 %s，归档表 %s；请换用与源表结构一致的归档表",
				i+1, src[i].Name, dst[i].Name)
		}
		srcType := src[i].ColumnType
		if srcType == "" {
			srcType = src[i].DataType
		}
		dstType := dst[i].ColumnType
		if dstType == "" {
			dstType = dst[i].DataType
		}
		if !strings.EqualFold(srcType, dstType) {
			return fmt.Errorf("列 %s 类型不一致：源表 %s，归档表 %s；类型不同会在写入时被静默转换，请换用与源表结构一致的归档表",
				src[i].Name, srcType, dstType)
		}
	}
	return nil
}

// isGeneratedColumn 判断是否为生成列（虚拟或存储），这类列不能显式插入。
func isGeneratedColumn(extra string) bool {
	u := strings.ToUpper(extra)
	return strings.Contains(u, "VIRTUAL GENERATED") || strings.Contains(u, "STORED GENERATED")
}

// insertableColumns 返回可插入的列名（带反引号），过滤掉生成列。
func insertableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	cols, err := tableColumns(ctx, db, table)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if !isGeneratedColumn(c.Extra) {
			out = append(out, qIdent(c.Name))
		}
	}
	return out, nil
}

// PrimaryKeyInfo 是探测到的主键信息。
type PrimaryKeyInfo struct {
	Column   string
	DataType string
}

// listPrimaryKeys 读取表的主键列（按主键内顺序）。只如实反映数据库，不做业务判断：
// 返回空切片表示没有主键，返回多列表示复合主键。
func listPrimaryKeys(ctx context.Context, db *sql.DB, table string) ([]PrimaryKeyInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT k.COLUMN_NAME, c.DATA_TYPE
		FROM information_schema.TABLE_CONSTRAINTS tc
		JOIN information_schema.KEY_COLUMN_USAGE k
		  ON k.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
		 AND k.TABLE_SCHEMA = tc.TABLE_SCHEMA
		 AND k.TABLE_NAME = tc.TABLE_NAME
		JOIN information_schema.COLUMNS c
		  ON c.TABLE_SCHEMA = k.TABLE_SCHEMA
		 AND c.TABLE_NAME = k.TABLE_NAME
		 AND c.COLUMN_NAME = k.COLUMN_NAME
		WHERE tc.TABLE_SCHEMA = DATABASE()
		  AND tc.TABLE_NAME = ?
		  AND tc.CONSTRAINT_TYPE = 'PRIMARY KEY'
		ORDER BY k.ORDINAL_POSITION`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []PrimaryKeyInfo
	for rows.Next() {
		var p PrimaryKeyInfo
		if err := rows.Scan(&p.Column, &p.DataType); err != nil {
			return nil, err
		}
		cols = append(cols, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}

// probePrimaryKey 探测表的主键（只认单列主键）。
func probePrimaryKey(ctx context.Context, db *sql.DB, table string) (*PrimaryKeyInfo, error) {
	cols, err := listPrimaryKeys(ctx, db, table)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("表 %s 没有主键，本工具要求有单列整型主键", table)
	}
	if len(cols) > 1 {
		return nil, fmt.Errorf("表 %s 是复合主键（%d 列），本工具暂不支持", table, len(cols))
	}
	return &cols[0], nil
}

// chooseAutoIncrementKey 决定全表模式用哪一列对齐自增值。
// 全表模式只做改名，本来不需要主键；只有"对齐自增值"这一步需要整型单列主键。
// 探测不到（无主键、复合主键、非整型主键）时返回空列名和跳过原因，
// 由调用方打印提示后继续改名，而不是把整表归档卡住。
// 配置了 key_column 却与实际主键不一致，属于配置错误，仍然直接报错。
func chooseAutoIncrementKey(ctx context.Context, db *sql.DB, table, configured string) (string, string, error) {
	cols, err := listPrimaryKeys(ctx, db, table)
	if err != nil {
		return "", "", err
	}
	if len(cols) == 0 {
		if configured != "" {
			return "", "", fmt.Errorf("配置了 key_column=%s，但表 %s 没有主键", configured, table)
		}
		return "", "表没有主键", nil
	}
	if len(cols) > 1 {
		if configured != "" {
			return "", "", fmt.Errorf("配置了 key_column=%s，但表 %s 是 %d 列复合主键", configured, table, len(cols))
		}
		return "", fmt.Sprintf("复合主键（%d 列）", len(cols)), nil
	}
	pk := cols[0]
	if configured != "" && !strings.EqualFold(configured, pk.Column) {
		return "", "", fmt.Errorf("配置的主键 %s 与表实际主键 %s 不一致", configured, pk.Column)
	}
	if NumericKindFromDBType(pk.DataType) != NumericInteger {
		return "", fmt.Sprintf("主键 %s 的类型是 %s", pk.Column, pk.DataType), nil
	}
	return pk.Column, "", nil
}

// judgePrimaryKeyUsable 判断探测到的主键能否用于分批游标。
// 只接受整型单列主键，其他类型明确拒绝并给出处置建议。
func judgePrimaryKeyUsable(configured string, pk *PrimaryKeyInfo) (string, error) {
	if pk == nil {
		return "", fmt.Errorf("未探测到主键")
	}
	if configured != "" && !strings.EqualFold(configured, pk.Column) {
		return "", fmt.Errorf("配置的主键 %s 与表实际主键 %s 不一致", configured, pk.Column)
	}
	if NumericKindFromDBType(pk.DataType) != NumericInteger {
		return "", fmt.Errorf("主键 %s 的类型是 %s，不是整型；分批游标要求整型单列主键，请改用整型主键或联系维护方扩展支持", pk.Column, pk.DataType)
	}
	return pk.Column, nil
}

// DependencyInfo 描述对源表的一个外部依赖。
type DependencyInfo struct {
	Kind string // foreign_key 或 view
	Name string
	Ref  string
}

// probeDependencies 探测引用源表的外键和视图。全表模式改名前必须确认无依赖。
func probeDependencies(ctx context.Context, db *sql.DB, table string) ([]DependencyInfo, error) {
	var deps []DependencyInfo
	fkRows, err := db.QueryContext(ctx, `
		SELECT TABLE_NAME, CONSTRAINT_NAME
		FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE()
		  AND REFERENCED_TABLE_NAME = ?`, table)
	if err != nil {
		return nil, err
	}
	for fkRows.Next() {
		var d DependencyInfo
		if err := fkRows.Scan(&d.Name, &d.Ref); err != nil {
			fkRows.Close()
			return nil, err
		}
		d.Kind = "foreign_key"
		deps = append(deps, d)
	}
	fkRows.Close()
	if err := fkRows.Err(); err != nil {
		return nil, err
	}

	viewRows, err := db.QueryContext(ctx, `
		SELECT TABLE_NAME, VIEW_DEFINITION
		FROM information_schema.VIEWS
		WHERE TABLE_SCHEMA = DATABASE()
		  AND VIEW_DEFINITION LIKE ?`, "%"+table+"%")
	if err != nil {
		return nil, err
	}
	for viewRows.Next() {
		var d DependencyInfo
		if err := viewRows.Scan(&d.Name, &d.Ref); err != nil {
			viewRows.Close()
			return nil, err
		}
		d.Kind = "view"
		deps = append(deps, d)
	}
	viewRows.Close()
	if err := viewRows.Err(); err != nil {
		return nil, err
	}
	return deps, nil
}

// judgeDependenciesAllowed 判断依赖是否允许改名。发现任一依赖就拒绝。
func judgeDependenciesAllowed(deps []DependencyInfo) error {
	if len(deps) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("发现对源表的外部依赖，改名会影响这些对象，请先处置：")
	for _, d := range deps {
		sb.WriteString(fmt.Sprintf("\n  - %s: %s (关联 %s)", d.Kind, d.Name, d.Ref))
	}
	return fmt.Errorf("%s", sb.String())
}

// probeTableTriggers 探测源表上的触发器。
// CREATE TABLE ... LIKE 不复制触发器，改名后触发器跟随原表进归档表，
// 而顶替原表名的空表没有任何触发器——业务继续写入原表名时，审计、同步之类的逻辑会静默失效。
func probeTableTriggers(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT TRIGGER_NAME, ACTION_TIMING, EVENT_MANIPULATION
		FROM information_schema.TRIGGERS
		WHERE EVENT_OBJECT_SCHEMA = DATABASE() AND EVENT_OBJECT_TABLE = ?
		ORDER BY TRIGGER_NAME`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, timing, event string
		if err := rows.Scan(&name, &timing, &event); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s（%s %s）", name, timing, event))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// probeOutboundForeignKeys 探测源表自身向外引用的外键（注意方向：这里查的是本表引用别人）。
// probeDependencies 查的是别人引用本表，用于判断改名会不会牵连别的对象；
// 这里查的是本表的外键定义会不会因 CREATE TABLE ... LIKE 不复制而丢失。
func probeOutboundForeignKeys(ctx context.Context, db *sql.DB, table string) ([]DependencyInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT CONSTRAINT_NAME, REFERENCED_TABLE_NAME
		FROM information_schema.KEY_COLUMN_USAGE
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME = ?
		  AND REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY CONSTRAINT_NAME`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DependencyInfo
	for rows.Next() {
		var d DependencyInfo
		if err := rows.Scan(&d.Name, &d.Ref); err != nil {
			return nil, err
		}
		d.Kind = "outbound_foreign_key"
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// judgeRenameSideEffects 判断改名会让新表丢掉哪些对象。
// 只列事实并给出处置建议，是否继续由调用方按 -force 决定。
func judgeRenameSideEffects(triggers []string, outFKs []DependencyInfo) error {
	if len(triggers) == 0 && len(outFKs) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("源表上存在改名后新表不会带走的对象（CREATE TABLE ... LIKE 只复制列与索引，不复制触发器与外键定义）：")
	for _, tr := range triggers {
		sb.WriteString(fmt.Sprintf("\n  - 触发器: %s", tr))
	}
	for _, fk := range outFKs {
		sb.WriteString(fmt.Sprintf("\n  - 外键: %s（引用 %s）", fk.Name, fk.Ref))
	}
	sb.WriteString("\n改名后这些对象会留在归档表上，顶替原表名的新空表没有它们：")
	sb.WriteString("\n触发器对应的业务逻辑不再执行，外键约束也不再拦截孤儿数据。")
	sb.WriteString("\n如确认可以接受，请自行在新表上重建，或加 -force 明确承担该后果后继续")
	return fmt.Errorf("%s", sb.String())
}

// tableExists 判断表是否存在于当前库。
func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// judgeArchiveTableFree 判断归档表是否不存在（存在则拒绝，不覆盖）。
func judgeArchiveTableFree(exists bool, archiveTable string) error {
	if exists {
		return fmt.Errorf("归档表 %s 已存在，为避免覆盖既有数据已停止；请换一个归档表名或先确认处置旧表", archiveTable)
	}
	return nil
}

// sourceAutoIncrement 读取源表当前的自增计数器；表中没有自增列时返回 0。
// 对齐自增值时要和它取大：删过历史行时 MAX(主键) 会小于计数器，
// 只按 MAX+1 对齐会让新表重新发放已经用过的 id。
func sourceAutoIncrement(ctx context.Context, db *sql.DB, table string) (int64, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT AUTO_INCREMENT FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// maxKeyValue 读取主键最大值，用于全表模式给新空表对齐自增值。
func maxKeyValue(ctx context.Context, db *sql.DB, table, keyCol string) (int64, error) {
	var v sql.NullInt64
	q := fmt.Sprintf("SELECT MAX(%s) FROM %s", qIdent(keyCol), qIdent(table))
	if err := db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil // 空表
	}
	return v.Int64, nil
}
