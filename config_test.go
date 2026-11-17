package dbmigration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "migration.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	path := writeTempConfig(t, `
database:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db"
tables:
- name: orders
  mode: conditional
  condition: "status = 1"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.MaxOpenConns != 20 {
		t.Errorf("默认最大连接数错误: %d", cfg.Database.MaxOpenConns)
	}
	if cfg.Output.Dir != "./migration-reports" || cfg.Checkpoint.Dir != "./migration-checkpoints" {
		t.Errorf("默认目录错误: %+v", cfg)
	}
	tc := &cfg.Tables[0]
	if !tc.UseHistoryTimestamp() {
		t.Error("条件迁移历史表默认应带时间戳")
	}
	if !tc.Verify.UseRowCount() {
		t.Error("默认应开启记录数比对")
	}
	if tc.Verify.RowCount != nil {
		t.Error("未显式配置 row_count 时应保持未设置状态，由 UseRowCount 决定默认值")
	}
	if tc.BatchSize != 2000 {
		t.Errorf("默认批大小错误: %d", tc.BatchSize)
	}
	if tc.Verify.AbsTolerance <= 0 || tc.Verify.RelTolerance <= 0 {
		t.Error("浮点容差默认值应大于0")
	}
}

func TestLoadConfigHistoryTimestampFalse(t *testing.T) {
	path := writeTempConfig(t, `
database:
  dsn: "x"
tables:
- name: orders
  mode: conditional
  condition: "1=1"
  history_timestamp: false
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tables[0].UseHistoryTimestamp() {
		t.Error("显式 false 应关闭时间戳")
	}
}

func TestLoadConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"缺DSN", "tables:/n- name: t\n  mode: full_table\n"},
		{"无表", "database:/n  dsn: x\n"},
		{"模式非法", "database:/n  dsn: x\ntables:/n- name: t\n  mode: bad\n"},
		{"条件模式缺条件", "database:/n  dsn: x\ntables:/n- name: t\n  mode: conditional\n"},
		{"全表模式带条件", "database:/n  dsn: x\ntables:/n- name: t\n  mode: full_table\n  condition: '1=1'\n"},
		{"表名非法", "database:/n  dsn: x\ntables:/n- name: 'bad-name'\n  mode: full_table\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTempConfig(t, c.yaml)
			if _, err := LoadConfig(path); err == nil {
				t.Error("应当校验失败")
			}
		})
	}
}

func TestLoadConfigLegacySumTolerance(t *testing.T) {
	path := writeTempConfig(t, `
database:
  dsn: x
tables:
- name: t
  mode: full_table
  verify:
    sum_tolerance: 0.00005
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tables[0].Verify.AbsTolerance != 0.00005 {
		t.Errorf("旧配置 sum_tolerance 应映射到绝对容差: %v", cfg.Tables[0].Verify.AbsTolerance)
	}
}

// boolPtr 便于构造 *bool 配置项。
func boolPtr(b bool) *bool { return &b }

// TestRowCountDefaultsOnEvenWithOtherChecks 是 P0-3 的回归测试。
// 旧实现只在“所有核对项都没配”时才默认开启记录数比对，导致配了求和字段却漏写
// row_count 时，记录数比对被静默跳过，违反“默认对迁移前后记录数做统计比较”的要求。
func TestRowCountDefaultsOnEvenWithOtherChecks(t *testing.T) {
	path := writeTempConfig(t, `
database:
  dsn: x
tables:
- name: orders
  mode: full_table
  verify:
    numeric_sum_fields: [amount]
    sample_rounds: 2
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tables[0].Verify.UseRowCount() {
		t.Error("配置了求和与抽样字段时，记录数比对仍应默认开启")
	}
}

func TestRowCountExplicitFalse(t *testing.T) {
	path := writeTempConfig(t, `
database:
  dsn: x
tables:
- name: orders
  mode: full_table
  verify:
    row_count: false
    numeric_sum_fields: [amount]
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tables[0].Verify.UseRowCount() {
		t.Error("显式 row_count: false 应关闭记录数比对")
	}
}

// TestTableNameTooLong 是 P1-10 的回归测试：派生的历史表/临时表名超过 64 字符必须在校验阶段拦截。
func TestTableNameTooLong(t *testing.T) {
	longName := "t_"
	for i := 0; i < 50; i++ {
		longName += "x"
	}
	yaml := "database:\n  dsn: x\ntables:\n- name: " + longName + "\n  mode: conditional\n  condition: '1=1'\n"
	if _, err := LoadConfig(writeTempConfig(t, yaml)); err == nil {
		t.Fatal("表名过长应当校验失败")
	}
}

// TestStrictModeConfig 覆盖 P2-4：严格模式策略可配置，默认仍为 require。
func TestStrictModeConfig(t *testing.T) {
	base := "database:\n  dsn: x\n"
	tables := "tables:\n- name: t\n  mode: full_table\n"

	cfg, err := LoadConfig(writeTempConfig(t, base+tables))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.StrictMode != StrictModeRequire {
		t.Errorf("未配置时严格模式策略应为 require，实际 %q", cfg.Database.StrictMode)
	}

	cfg, err = LoadConfig(writeTempConfig(t, base+"  strict_mode: WARN\n"+tables))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.StrictMode != StrictModeWarn {
		t.Errorf("取值应统一小写，实际 %q", cfg.Database.StrictMode)
	}

	// 拼错取值必须报错，不能静默回退到 require 或 off。
	if _, err := LoadConfig(writeTempConfig(t, base+"  strict_mode: warm\n"+tables)); err == nil {
		t.Error("非法的 strict_mode 取值应当校验失败")
	}
}

// TestJudgeStrictMode 校验三种策略的判定结果。
func TestJudgeStrictMode(t *testing.T) {
	nonStrict := "NO_ENGINE_SUBSTITUTION"
	strict := "STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION"

	if notice, err := judgeStrictMode(StrictModeRequire, strict, strict); err != nil || notice != "" {
		t.Errorf("严格模式下 require 应静默通过: notice=%q err=%v", notice, err)
	}
	// 默认策略在非严格模式下必须阻断。
	if _, err := judgeStrictMode(StrictModeRequire, nonStrict, nonStrict); err == nil {
		t.Error("require 策略遇到非严格模式应当报错")
	}
	// warn 策略给出可留痕的告警但不阻断。
	notice, err := judgeStrictMode(StrictModeWarn, nonStrict, nonStrict)
	if err != nil {
		t.Errorf("warn 策略不应报错: %v", err)
	}
	if !strings.Contains(notice, "warn") {
		t.Errorf("warn 策略应返回明确告警文案，实际 %q", notice)
	}
	// off 策略直接放行，但仍返回留痕文案。
	notice, err = judgeStrictMode(StrictModeOff, nonStrict, nonStrict)
	if err != nil || notice == "" {
		t.Errorf("off 策略应放行并返回留痕文案: notice=%q err=%v", notice, err)
	}
	if !strings.Contains(notice, "strict_mode=off") {
		t.Errorf("off 策略文案应说明配置来源，实际 %q", notice)
	}
}

// TestLongestGeneratedName 校验派生名长度计算的边界值。
func TestLongestGeneratedName(t *testing.T) {
	tc := &TableConfig{Name: "orders", HistorySuffix: "_history"}
	got := longestGeneratedName(tc)
	// orders_history_000000000000000_trash_00000000 最长
	want := len("orders" + "_history" + "_" + "000000000000000" + "_trash_" + "00000000")
	if len(got) != want {
		t.Errorf("最长派生表名长度计算错误: got %d(%q) want %d", len(got), got, want)
	}
	// 恰好 64 字符应可通过，65 字符必须失败。
	base := maxTableNameLen - want + len("orders")
	tcOK := &TableConfig{Name: strings.Repeat("a", base), HistorySuffix: "_history", Mode: "conditional", Condition: "1=1", KeyColumn: "id"}
	if err := validateTable(tcOK); err != nil {
		t.Errorf("长度恰好 64 的表名不应报错: %v", err)
	}
	tcBad := &TableConfig{Name: strings.Repeat("a", base+1), HistorySuffix: "_history", Mode: "conditional", Condition: "1=1", KeyColumn: "id"}
	if err := validateTable(tcBad); err == nil {
		t.Error("长度超过 64 的表名应当报错")
	}
}
