// 配置定义与加载。
//
// 本工具只做两件事：
//   - full_table：整表归档，改名 + 建空表，前提是该表不再写入。
//   - conditional：按条件把老数据搬到归档表，校验通过后按已校验主键清单删源。
//
// 配置校验尽量在启动阶段完成，避免跑到一半才因为主键不存在、条件为空等问题失败。
package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ModeFullTable   = "full_table"
	ModeConditional = "conditional"
)

// 标识符只允许字母、数字、下划线，且不能以数字开头。
// 表名、列名会拼进 SQL，必须先过这一关。
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Config struct {
	Database   DBConfig      `yaml:"database"`
	Output     OutputConfig  `yaml:"output"`
	Checkpoint CheckpointCfg `yaml:"checkpoint"`
	Tables     []TableCfg    `yaml:"tables"`
}

type DBConfig struct {
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns"`
}

type OutputConfig struct {
	Dir string `yaml:"dir"`
}

type CheckpointCfg struct {
	Dir string `yaml:"dir"`
}

type TableCfg struct {
	Name         string    `yaml:"name"`          // 源表名
	Mode         string    `yaml:"mode"`          // full_table 或 conditional
	ArchiveTable string    `yaml:"archive_table"` // 归档表名
	Condition    string    `yaml:"condition"`     // conditional 模式的 WHERE 条件
	KeyColumn    string    `yaml:"key_column"`    // 主键列；留空则自动探测
	BatchSize    int       `yaml:"batch_size"`    // 每批行数
	Verify       VerifyCfg `yaml:"verify"`        // 数值校验字段
}

type VerifyCfg struct {
	// NumericFields 需要做数值校验的列名。类型自动探测，按类型决定精确或容差比对。
	NumericFields []string `yaml:"numeric_fields"`
	// BlockSize 主键分块大小，用于分块计数校验，定位差异落在哪一段。
	BlockSize int64 `yaml:"block_size"`
	// BlockCountMax 分块数量上限。主键跨度可能极大（雪花 ID、时间戳做主键时跨度可达 1e18），
	// 若严格按 BlockSize 切块会产生天文数字个分块，直接把进程撑爆。
	// 超过上限时自动放大块宽，宁可定位精度变粗也不让校验跑不起来。
	BlockCountMax int `yaml:"block_count_max"`
	// AbsTolerance 浮点绝对容差，默认 0.0001。
	AbsTolerance float64 `yaml:"abs_tolerance"`
	// RelTolerance 浮点相对容差，默认 0.00001。
	RelTolerance float64 `yaml:"rel_tolerance"`
	// SampleRounds 抽样轮数，默认 3；设为 0 关闭抽样。
	// 用指针区分"未配置"和"显式配 0"，避免默认值覆盖关闭意图。
	SampleRounds *int `yaml:"sample_rounds"`
	// SampleSize 每轮抽样条数，默认 50；总样本数 = SampleRounds × SampleSize。
	SampleSize *int `yaml:"sample_size"`
}

// Rounds 返回实际抽样轮数（含默认值）。
func (v VerifyCfg) Rounds() int {
	if v.SampleRounds == nil {
		return 3
	}
	return *v.SampleRounds
}

// PerRound 返回实际每轮抽样条数（含默认值）。
func (v VerifyCfg) PerRound() int {
	if v.SampleSize == nil {
		return 50
	}
	return *v.SampleSize
}

// SampleEnabled 是否开启抽样。
func (v VerifyCfg) SampleEnabled() bool {
	return v.Rounds() > 0 && v.PerRound() > 0
}

// LoadConfig 读取并校验配置文件。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() error {
	if c.Database.MaxOpenConns <= 0 {
		c.Database.MaxOpenConns = 10
	}
	if c.Database.MaxIdleConns <= 0 {
		c.Database.MaxIdleConns = 5
	}
	if c.Output.Dir == "" {
		c.Output.Dir = "./migration-output"
	}
	if c.Checkpoint.Dir == "" {
		c.Checkpoint.Dir = "./migration-checkpoints"
	}
	for i := range c.Tables {
		t := &c.Tables[i]
		if t.BatchSize <= 0 {
			t.BatchSize = 5000
		}
		if t.Verify.BlockSize <= 0 {
			t.Verify.BlockSize = 100000
		}
		if t.Verify.BlockCountMax <= 0 {
			t.Verify.BlockCountMax = 1000
		}
		if t.Verify.BlockCountMax > 100000 {
			return fmt.Errorf("tables[%d].verify.block_count_max 不能超过 100000", i)
		}
		if t.Verify.AbsTolerance == 0 {
			t.Verify.AbsTolerance = 0.0001
		}
		if t.Verify.RelTolerance == 0 {
			t.Verify.RelTolerance = 0.00001
		}
		// 抽样参数：未配置时用方法里的默认值；这里只把显式配置值校验到合法范围。
		rounds := t.Verify.Rounds()
		perRound := t.Verify.PerRound()
		if rounds < 0 {
			return fmt.Errorf("tables[%d].verify.sample_rounds 不能为负数", i)
		}
		if perRound < 0 {
			return fmt.Errorf("tables[%d].verify.sample_size 不能为负数", i)
		}
		if rounds > 100 {
			return fmt.Errorf("tables[%d].verify.sample_rounds 不能超过 100", i)
		}
		if perRound > 10000 {
			return fmt.Errorf("tables[%d].verify.sample_size 不能超过 10000", i)
		}
		// 回写为具体值，便于指纹计算稳定（未配置也固定成默认值）。
		t.Verify.SampleRounds = &rounds
		t.Verify.SampleSize = &perRound
	}
	return nil
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Database.DSN) == "" {
		return fmt.Errorf("database.dsn 不能为空")
	}
	if len(c.Tables) == 0 {
		return fmt.Errorf("tables 至少要配置一张表")
	}
	seen := make(map[string]bool)
	for i, t := range c.Tables {
		label := fmt.Sprintf("tables[%d]", i)
		if err := t.validate(label); err != nil {
			return err
		}
		key := t.Name + "|" + t.ArchiveTable
		if seen[key] {
			return fmt.Errorf("%s 与前面的表配置重复: %s -> %s", label, t.Name, t.ArchiveTable)
		}
		seen[key] = true
	}
	return nil
}

func (t *TableCfg) validate(label string) error {
	if !identRe.MatchString(t.Name) {
		return fmt.Errorf("%s.name 不是合法标识符: %q", label, t.Name)
	}
	if !identRe.MatchString(t.ArchiveTable) {
		return fmt.Errorf("%s.archive_table 不是合法标识符: %q", label, t.ArchiveTable)
	}
	if t.Name == t.ArchiveTable {
		return fmt.Errorf("%s 源表和归档表不能同名: %s", label, t.Name)
	}
	if t.KeyColumn != "" && !identRe.MatchString(t.KeyColumn) {
		return fmt.Errorf("%s.key_column 不是合法标识符: %q", label, t.KeyColumn)
	}
	for _, f := range t.Verify.NumericFields {
		if !identRe.MatchString(f) {
			return fmt.Errorf("%s.verify.numeric_fields 含非法列名: %q", label, f)
		}
	}
	switch t.Mode {
	case ModeFullTable:
		if strings.TrimSpace(t.Condition) != "" {
			return fmt.Errorf("%s 全表模式不允许配置 condition", label)
		}
	case ModeConditional:
		if strings.TrimSpace(t.Condition) == "" {
			return fmt.Errorf("%s 条件模式必须配置 condition", label)
		}
		if err := validateCondition(t.Condition); err != nil {
			return fmt.Errorf("%s.condition 不合法: %w", label, err)
		}
	default:
		return fmt.Errorf("%s.mode 只能是 %s 或 %s，当前为 %q", label, ModeFullTable, ModeConditional, t.Mode)
	}
	return nil
}

// validateCondition 拦截明显危险的 WHERE 条件。
// condition 会被直接拼进 SQL，只允许运维自己写的可信条件，这里做最小白名单检查。
func validateCondition(cond string) error {
	upper := strings.ToUpper(cond)
	if strings.Contains(cond, ";") {
		return fmt.Errorf("不允许分号")
	}
	if strings.Contains(cond, "--") || strings.Contains(cond, "/*") {
		return fmt.Errorf("不允许注释符")
	}
	if strings.Contains(upper, " UNION ") || strings.Contains(upper, "SELECT ") {
		return fmt.Errorf("不允许子查询或 UNION")
	}
	return nil
}
