package dbmigration

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func OpenDB(cfg DBConfigV2) (*sql.DB, error) {
	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(cfg.ConnMaxLifeSeconds) * time.Second)

	// 设置元数据锁等待超时，避免改表操作无限期阻塞。
	if cfg.LockWaitTimeoutSeconds > 0 {
		ctx0, cancel0 := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = db.ExecContext(ctx0, fmt.Sprintf("SET SESSION lock_wait_timeout = %d", cfg.LockWaitTimeoutSeconds))
		cancel0()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return db, nil
}

// qIdent 返回带反引号的标识符。配置加载时已用 identRe 校验，内部拼接的表名也经校验。
func qIdent(name string) string {
	if !identRe.MatchString(name) {
		panic(fmt.Sprintf("非法标识符: %q", name))
	}
	return "`" + name + "`"
}

// 严格模式检查策略。
const (
	// StrictModeRequire 是默认策略：数据库未开启严格模式时拒绝执行迁移。
	StrictModeRequire = "require"
	// StrictModeWarn 只告警不阻断，用于目标库由 DBA 统一管控、不允许改会话变量的场景。
	StrictModeWarn = "warn"
	// StrictModeOff 完全跳过检查，风险由调用方承担。
	StrictModeOff = "off"
)

// ensureStrictMode 按策略确认数据库开启了严格模式。
// 非严格模式下，超长字符串会被静默截断、非空约束违规会退化为默认值，
// 迁移后的记录数与求和都可能“正常通过”，坏数据却已经写进历史表。
// 这类问题一旦发生不可逆，因此默认在迁移启动时就拦截，而不是等抽样去发现。
//
// 返回值 notice 非空表示“本次运行没有严格模式保护”，调用方必须写入报告留痕，
// 不能只打印一行日志了事——事后追查时要能从结果文件看出当时的运行前提。
func ensureStrictMode(ctx context.Context, db *sql.DB, mode string) (string, error) {
	switch mode {
	case StrictModeOff:
		return judgeStrictMode(mode, "", "")
	case "", StrictModeRequire, StrictModeWarn:
		// 继续执行实际检查。
	default:
		return "", fmt.Errorf("未知的 strict_mode %q (可选 require|warn|off)", mode)
	}

	var sessionMode string
	if err := db.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&sessionMode); err != nil {
		return "", fmt.Errorf("读取 sql_mode 失败: %w", err)
	}
	if isStrictSQLMode(sessionMode) {
		return judgeStrictMode(mode, sessionMode, sessionMode)
	}
	var globalMode string
	_ = db.QueryRowContext(ctx, "SELECT @@GLOBAL.sql_mode").Scan(&globalMode)
	return judgeStrictMode(mode, sessionMode, globalMode)
}

// isStrictSQLMode 判断一组 sql_mode 是否包含严格模式标志。
func isStrictSQLMode(m string) bool {
	u := strings.ToUpper(m)
	return strings.Contains(u, "STRICT_TRANS_TABLES") || strings.Contains(u, "STRICT_ALL_TABLES")
}

// judgeStrictMode 是纯决策函数，把数据库探测结果与配置策略合成为最终结论。
// 单独拆出来便于单元测试覆盖三种策略，不必依赖真实数据库。
func judgeStrictMode(mode, sessionMode, globalMode string) (string, error) {
	if mode == StrictModeOff {
		return "已显式关闭严格模式检查（database.strict_mode=off）：超长字符串可能被静默截断而记录数与求和仍显示通过", nil
	}
	if isStrictSQLMode(sessionMode) {
		return "", nil
	}
	detail := fmt.Sprintf("数据库未开启严格模式（当前会话 sql_mode=%q，全局 sql_mode=%q）。"+
		"非严格模式下超长字符串会被静默截断，数据损坏后记录数与求和仍可能显示通过",
		sessionMode, globalMode)
	if mode == StrictModeWarn {
		return detail + "；已按 database.strict_mode=warn 继续执行，本次迁移结果不具备严格模式下的可信度", nil
	}
	return "", fmt.Errorf("%s。请在 DSN 中追加 sql_mode=STRICT_ALL_TABLES 后重试；"+
		"若目标库不允许修改会话变量，可显式配置 database.strict_mode: warn 并自行承担截断风险", detail)
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
		table).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ColumnMeta 是单列的元数据，用于类型感知的核对与插入列过滤。
type ColumnMeta struct {
	Name     string
	DataType string
	Extra    string
	Kind     NumericKind
}

// tableColumns 读取表的所有列元数据（按字段顺序）。
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
		c.Kind = NumericKindFromDBType(c.DataType)
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("表 %s 无列信息或表不存在", table)
	}
	return cols, nil
}

// isGeneratedColumn 判断是否为生成列（虚拟/存储），这类列不能显式插入。
func isGeneratedColumn(extra string) bool {
	u := strings.ToUpper(extra)
	return strings.Contains(u, "VIRTUAL GENERATED") || strings.Contains(u, "STORED GENERATED")
}

// insertableColumns 返回可插入列名（已 quote），过滤掉生成列。
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

// columnKindMap 返回 列名(小写) -> 数值类别 的映射。
func columnKindMap(cols []ColumnMeta) map[string]NumericKind {
	m := make(map[string]NumericKind, len(cols))
	for _, c := range cols {
		m[strings.ToLower(c.Name)] = c.Kind
	}
	return m
}

const shortRandLetters = "abcdefghijklmnopqrstuvwxyz0123456789"

func shortRand(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = shortRandLetters[rand.Intn(len(shortRandLetters))]
	}
	return string(b)
}
