package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// MigrateConfig 是整个工具的配置根。
type MigrateConfig struct {
	Database   DBConfigV2       `yaml:"database"`
	Output     OutputConfig     `yaml:"output"`
	Checkpoint CheckpointConfig `yaml:"checkpoint"`
	Tables     []TableConfig    `yaml:"tables"`
}

// DBConfigV2 是数据库连接池配置。
type DBConfigV2 struct {
	DSN                    string `yaml:"dsn"`
	MaxOpenConns           int    `yaml:"max_open_conns"`
	MaxIdleConns           int    `yaml:"max_idle_conns"`
	ConnMaxLifeSeconds     int    `yaml:"conn_max_life_seconds"`
	LockWaitTimeoutSeconds int    `yaml:"lock_wait_timeout_seconds"`
	StrictMode             string `yaml:"strict_mode"` // require(默认) | warn | off
}

// OutputConfig 定义结果文件输出位置。
type OutputConfig struct {
	Dir string `yaml:"dir"`
}

// CheckpointConfig 定义断点续跑进度文件位置。
type CheckpointConfig struct {
	Dir string `yaml:"dir"`
}

// TableConfig 是单张表的迁移配置。
type TableConfig struct {
	Name             string       `yaml:"name"`
	Mode             string       `yaml:"mode"` // full_table | conditional
	Condition        string       `yaml:"condition"`
	HistorySuffix    string       `yaml:"history_suffix"`
	HistoryTimestamp *bool        `yaml:"history_timestamp"` // 条件迁移历史表名是否带批次时间戳，默认 true
	KeyColumn        string       `yaml:"key_column"`
	BatchSize        int          `yaml:"batch_size"`
	OverwriteHistory bool         `yaml:"overwrite_history"`
	RetentionDays    int          `yaml:"retention_days"` // 历史表保留天数，用于生命周期登记
	Verify           VerifyConfig `yaml:"verify"`
}

// UseRowCount 返回是否执行记录数比对。
// 未显式配置时默认开启，这是“默认对迁移前后记录数做统计比较”要求的实现点：
// 即使同时配置了求和字段、抽样等其他核对项，记录数比对也不会被静默跳过。
func (v *VerifyConfig) UseRowCount() bool {
	if v.RowCount == nil {
		return true
	}
	return *v.RowCount
}

// UseHistoryTimestamp 返回条件迁移是否使用带时间戳的历史表名。默认 true。
func (t *TableConfig) UseHistoryTimestamp() bool {
	if t.HistoryTimestamp == nil {
		return true
	}
	return *t.HistoryTimestamp
}

// VerifyConfig 定义核对规则。
// 数值字段的类别（整数/定点/浮点）自动从表结构推断，不依赖人工填写，避免配错。
type VerifyConfig struct {
	RowCount         *bool    `yaml:"row_count"`      // 记录数比对，不填默认 true，显式 false 才关闭
	NonemptyCount    bool     `yaml:"nonempty_count"` // 指定字段非空数量比对
	MinMax           bool     `yaml:"min_max"`        // 指定字段最小值/最大值比对
	SampleRounds     int      `yaml:"sample_rounds"`  // 随机抽样轮数
	SampleSize       int      `yaml:"sample_size"`    // 每轮抽样条数
	NumericSumFields []string `yaml:"numeric_sum_fields"`
	NonemptyFields   []string `yaml:"nonempty_fields"`
	MinMaxFields     []string `yaml:"min_max_fields"`
	AbsTolerance     float64  `yaml:"abs_tolerance"` // 浮点绝对容差
	RelTolerance     float64  `yaml:"rel_tolerance"` // 浮点相对容差
	SumTolerance     float64  `yaml:"sum_tolerance"` // 兼容旧配置，作为绝对容差默认值
	// DetectSourceDrift 控制是否在复制结束后检测源表是否被业务写入，默认开启。
	// 复制窗口可能很长，期间业务改动会导致历史表不是源表的快照；
	// 该检测用轻量指纹（记录数/求和/非空数/最值）发现变化，属于“发现”而非“阻止”。
	DetectSourceDrift *bool `yaml:"detect_source_drift"`
}

// UseSourceDriftCheck 返回是否执行复制期源表漂移检测，未配置时默认开启。
func (v *VerifyConfig) UseSourceDriftCheck() bool {
	if v.DetectSourceDrift == nil {
		return true
	}
	return *v.DetectSourceDrift
}

var identRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func LoadConfig(path string) (*MigrateConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var cfg MigrateConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *MigrateConfig) applyDefaults() {
	if c.Database.MaxOpenConns <= 0 {
		c.Database.MaxOpenConns = 20
	}
	if c.Database.MaxIdleConns <= 0 {
		c.Database.MaxIdleConns = 10
	}
	if c.Database.ConnMaxLifeSeconds <= 0 {
		c.Database.ConnMaxLifeSeconds = 3600
	}
	if c.Database.LockWaitTimeoutSeconds <= 0 {
		c.Database.LockWaitTimeoutSeconds = 30
	}
	if strings.TrimSpace(c.Database.StrictMode) == "" {
		c.Database.StrictMode = StrictModeRequire
	} else {
		c.Database.StrictMode = strings.ToLower(strings.TrimSpace(c.Database.StrictMode))
	}
	if strings.TrimSpace(c.Output.Dir) == "" {
		c.Output.Dir = "./migration-reports"
	}
	if strings.TrimSpace(c.Checkpoint.Dir) == "" {
		c.Checkpoint.Dir = "./migration-checkpoints"
	}
	for i := range c.Tables {
		t := &c.Tables[i]
		if t.HistorySuffix == "" {
			t.HistorySuffix = "_history"
		}
		if t.KeyColumn == "" {
			t.KeyColumn = "id"
		}
		if t.BatchSize <= 0 {
			t.BatchSize = 2000
		}
		if t.RetentionDays <= 0 {
			t.RetentionDays = 365
		}
		if t.Verify.AbsTolerance <= 0 {
			if t.Verify.SumTolerance > 0 {
				t.Verify.AbsTolerance = t.Verify.SumTolerance
			} else {
				t.Verify.AbsTolerance = 1e-6
			}
		}
		if t.Verify.RelTolerance <= 0 {
			t.Verify.RelTolerance = 1e-9
		}
		if t.Verify.SampleRounds < 0 {
			t.Verify.SampleRounds = 0
		}
		if t.Verify.SampleSize < 0 {
			t.Verify.SampleSize = 0
		}
		if t.Verify.SampleRounds > 0 && t.Verify.SampleSize <= 0 {
			t.Verify.SampleSize = 100
		}
		// 注意：row_count 不再在此处“补写”为 true。
		// 旧实现只在“所有核对项都没配”时才默认开启，导致配了求和字段却漏写
		// row_count 时记录数比对被静默跳过。现在改为指针字段 + UseRowCount()，
		// 未配置即视为开启，显式 false 才关闭。
	}
}

func (c *MigrateConfig) validate() error {
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn 不能为空")
	}
	switch c.Database.StrictMode {
	case StrictModeRequire, StrictModeWarn, StrictModeOff:
	default:
		// 拼错取值时必须报错而不是回退到默认值：
		// 若把 "warm" 静默当成 require，现场只会看到“严格模式未开启”的拒绝执行，
		// 却找不到原因；反之若静默降级成 off，安全基线就被悄悄放开了。
		return fmt.Errorf("database.strict_mode 取值非法: %q (可选 require|warn|off)", c.Database.StrictMode)
	}
	if len(c.Tables) == 0 {
		return fmt.Errorf("tables 不能为空")
	}
	seen := make(map[string]bool, len(c.Tables))
	for i := range c.Tables {
		if err := validateTable(&c.Tables[i]); err != nil {
			return fmt.Errorf("tables[%d](%s): %w", i, c.Tables[i].Name, err)
		}
		if seen[c.Tables[i].Name] {
			return fmt.Errorf("表名 %s 重复配置", c.Tables[i].Name)
		}
		seen[c.Tables[i].Name] = true
	}
	return nil
}

func validateTable(t *TableConfig) error {
	if t.Name == "" || !identRe.MatchString(t.Name) {
		return fmt.Errorf("表名非法")
	}
	if !identRe.MatchString(t.HistorySuffix) {
		return fmt.Errorf("history_suffix 非法")
	}
	if !identRe.MatchString(t.KeyColumn) {
		return fmt.Errorf("key_column 非法")
	}
	switch t.Mode {
	case "full_table":
		if strings.TrimSpace(t.Condition) != "" {
			return fmt.Errorf("full_table 模式下 condition 必须为空")
		}
	case "conditional":
		if strings.TrimSpace(t.Condition) == "" {
			return fmt.Errorf("conditional 模式下 condition 不能为空")
		}
	default:
		return fmt.Errorf("不支持的 mode %q (full_table|conditional)", t.Mode)
	}
	for _, f := range t.Verify.NumericSumFields {
		if !identRe.MatchString(f) {
			return fmt.Errorf("numeric_sum_fields 含非法字段 %q", f)
		}
	}
	for _, f := range t.Verify.NonemptyFields {
		if !identRe.MatchString(f) {
			return fmt.Errorf("nonempty_fields 含非法字段 %q", f)
		}
	}
	for _, f := range t.Verify.MinMaxFields {
		if !identRe.MatchString(f) {
			return fmt.Errorf("min_max_fields 含非法字段 %q", f)
		}
	}
	if t.OverwriteHistory && t.Mode != "full_table" {
		return fmt.Errorf("overwrite_history 仅允许在 full_table 模式")
	}
	// 表名长度校验：MySQL 标识符上限 64 字符。
	// 迁移过程中会派生历史表、临时新表、覆盖用的回收表，任一超长都会在建表阶段失败，
	// 且报错不直观，因此在配置校验阶段拦截。
	if longest := longestGeneratedName(t); len(longest) > maxTableNameLen {
		return fmt.Errorf("生成的表名过长: %q 共 %d 字符，超过上限 %d；请缩短表名或 history_suffix",
			longest, len(longest), maxTableNameLen)
	}
	return nil
}

// maxTableNameLen 是 MySQL 标识符长度上限。
const maxTableNameLen = 64

// longestGeneratedName 返回该表配置下可能生成的最长表名。
// 覆盖：历史表（含批次时间戳）、全表迁移的临时新表、覆盖历史表用的回收表。
func longestGeneratedName(t *TableConfig) string {
	const timeStampLen = 15 // 20060102_150405
	const trashLen = 8      // 随机后缀
	candidates := []string{
		t.Name + t.HistorySuffix,
		t.Name + t.HistorySuffix + "_" + strings.Repeat("0", timeStampLen),
		t.Name + "_mig_new_" + strings.Repeat("0", timeStampLen),
		t.Name + t.HistorySuffix + "_" + strings.Repeat("0", timeStampLen) + "_trash_" + strings.Repeat("0", trashLen),
	}
	longest := candidates[0]
	for _, c := range candidates[1:] {
		if len(c) > len(longest) {
			longest = c
		}
	}
	return longest
}

// EnsureOutputDir 创建结果输出目录与进度目录。
func EnsureOutputDir(cfg *MigrateConfig) error {
	for _, dir := range []string{cfg.Output.Dir, cfg.Checkpoint.Dir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Clean(dir), 0o755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}
	return nil
}
