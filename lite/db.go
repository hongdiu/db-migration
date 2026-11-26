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
	Extra    string
}

// tableColumns 读取表的全部列（按定义顺序）。
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]ColumnMeta, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME, DATA_TYPE, IFNULL(EXTRA,'')
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
		if err := rows.Scan(&c.Name, &c.DataType, &c.Extra); err != nil {
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

// probePrimaryKey 探测表的主键（只认单列主键）。
func probePrimaryKey(ctx context.Context, db *sql.DB, table string) (*PrimaryKeyInfo, error) {
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
	if len(cols) == 0 {
		return nil, fmt.Errorf("表 %s 没有主键，本工具要求有单列整型主键", table)
	}
	if len(cols) > 1 {
		return nil, fmt.Errorf("表 %s 是复合主键（%d 列），本工具暂不支持", table, len(cols))
	}
	return &cols[0], nil
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
