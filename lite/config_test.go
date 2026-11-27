package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	return path
}

const validConfig = `
database:
  dsn: "user:pass@tcp(127.0.0.1:3306)/db?parseTime=true"
tables:
  - name: orders
    mode: conditional
    archive_table: orders_archive
    condition: "created_at < '2024-01-01'"
    key_column: id
    batch_size: 2000
    verify:
      numeric_fields: [amount]
`

// TestExampleConfigParses 确保仓库里的示例配置与代码字段同步，
// 避免文档改了字段名、示例却还在用旧名字这种"照抄就跑不起来"的问题。
func TestExampleConfigParses(t *testing.T) {
	cfg, err := LoadConfig("config.example.yaml")
	if err != nil {
		t.Fatalf("示例配置解析失败: %v", err)
	}
	if len(cfg.Tables) == 0 {
		t.Fatal("示例配置应至少包含一张表")
	}
	for _, tbl := range cfg.Tables {
		tols := tbl.Verify.Tolerances()
		if tols.For(NumericDouble).Abs != 1e-9 || tols.For(NumericDouble).Rel != 1e-12 {
			t.Fatalf("示例配置里的 DOUBLE 容差未被识别: %+v", tols)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeTempConfig(t, validConfig))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Database.MaxOpenConns != 10 || cfg.Database.MaxIdleConns != 5 {
		t.Fatalf("连接池默认值不对: %+v", cfg.Database)
	}
	if cfg.Output.Dir != "./migration-output" || cfg.Checkpoint.Dir != "./migration-checkpoints" {
		t.Fatalf("目录默认值不对: %+v %+v", cfg.Output, cfg.Checkpoint)
	}
	tbl := cfg.Tables[0]
	if tbl.BatchSize != 2000 {
		t.Fatalf("batch_size 应保留配置值 2000，实际 %d", tbl.BatchSize)
	}
	if tbl.Verify.BlockSize != 100000 {
		t.Fatalf("block_size 默认应为 100000，实际 %d", tbl.Verify.BlockSize)
	}
	if tbl.Verify.BlockCountMax != 1000 {
		t.Fatalf("block_count_max 默认应为 1000，实际 %d", tbl.Verify.BlockCountMax)
	}
	tols := tbl.Verify.Tolerances()
	if tols.For(NumericFloat).Abs != 0.0001 || tols.For(NumericFloat).Rel != 0.00001 {
		t.Fatalf("FLOAT 容差默认值不对: %+v", tols)
	}
	// DOUBLE 必须有自己的默认阈值，不能沿用 FLOAT 那一组。
	if tols.For(NumericDouble).Abs != 1e-9 || tols.For(NumericDouble).Rel != 1e-12 {
		t.Fatalf("DOUBLE 容差默认值不对: %+v", tols)
	}
	if tols.For(NumericDouble).Rel != 1e-12 || tols.For(NumericFloat).Rel != 0.00001 {
		t.Fatalf("按类型取阈值不对: %+v", tols)
	}
	if tbl.Verify.Rounds() != 3 || tbl.Verify.PerRound() != 50 || !tbl.Verify.SampleEnabled() {
		t.Fatalf("抽样默认值不对: rounds=%d size=%d", tbl.Verify.Rounds(), tbl.Verify.PerRound())
	}
}

func TestSampleConfigExplicitZero(t *testing.T) {
	content := `
database:
  dsn: "x"
tables:
  - name: a
    mode: conditional
    archive_table: ah
    condition: "id>0"
    verify:
      sample_rounds: 0
      sample_size: 0
`
	cfg, err := LoadConfig(writeTempConfig(t, content))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	v := cfg.Tables[0].Verify
	if v.Rounds() != 0 || v.PerRound() != 0 {
		t.Fatalf("显式 0 不应被默认值覆盖: rounds=%d size=%d", v.Rounds(), v.PerRound())
	}
	if v.SampleEnabled() {
		t.Fatal("显式 0 应关闭抽样")
	}
}

func TestSampleConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"轮数负数", "sample_rounds: -1", "不能为负数"},
		{"条数负数", "sample_size: -1", "不能为负数"},
		{"轮数过大", "sample_rounds: 1000", "不能超过 100"},
		{"条数过大", "sample_size: 99999", "不能超过 10000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := "database:\n  dsn: x\ntables:\n  - name: a\n    mode: conditional\n    archive_table: ah\n    condition: 'id>0'\n    verify:\n      " + tc.value + "\n"
			_, err := LoadConfig(writeTempConfig(t, content))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望错误含 %q，实际 %v", tc.wantErr, err)
			}
		})
	}
}

func TestToleranceExplicitZeroAndNegative(t *testing.T) {
	base := "database:\n  dsn: x\ntables:\n  - name: a\n    mode: conditional\n    archive_table: ah\n    condition: 'id>0'\n    verify:\n      "
	// 显式配 0 表示零容差（必须完全相等），不能被默认值覆盖。
	cfg, err := LoadConfig(writeTempConfig(t, base+"abs_tolerance: 0\n      rel_tolerance: 0\n      double_abs_tolerance: 0\n      double_rel_tolerance: 0\n"))
	if err != nil {
		t.Fatalf("显式 0 应合法: %v", err)
	}
	tols := cfg.Tables[0].Verify.Tolerances()
	for name, got := range map[string]float64{
		"FLOAT 绝对容差":  tols.For(NumericFloat).Abs,
		"FLOAT 相对容差":  tols.For(NumericFloat).Rel,
		"DOUBLE 绝对容差": tols.For(NumericDouble).Abs,
		"DOUBLE 相对容差": tols.For(NumericDouble).Rel,
	} {
		if got != 0 {
			t.Fatalf("%s 显式配 0 不应被默认值覆盖，实际 %g", name, got)
		}
	}
	// 四项负数都要报出来，并指出具体字段名。
	for _, key := range []string{"abs_tolerance", "rel_tolerance", "double_abs_tolerance", "double_rel_tolerance"} {
		_, err := LoadConfig(writeTempConfig(t, base+key+": -0.001\n"))
		if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "负数") {
			t.Fatalf("%s 配负数应报错并指出字段名，实际 %v", key, err)
		}
	}
}

func TestLoadConfigFullTable(t *testing.T) {
	content := `
database:
  dsn: "u:p@tcp(h:3306)/db"
tables:
  - name: logs
    mode: full_table
    archive_table: logs_archive
`
	cfg, err := LoadConfig(writeTempConfig(t, content))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Tables[0].Mode != ModeFullTable {
		t.Fatalf("模式不对: %s", cfg.Tables[0].Mode)
	}
}

func TestFullTableTempNameLength(t *testing.T) {
	base := "database:\n  dsn: x\ntables:\n  - name: "
	// 53 字符表名 + 11 字符临时后缀 = 64，刚好合法。
	name53 := strings.Repeat("a", 53)
	cfg, err := LoadConfig(writeTempConfig(t, base+name53+"\n    mode: full_table\n    archive_table: "+name53+"_archive\n"))
	if err != nil {
		t.Fatalf("64 字符的临时表名应允许: %v", err)
	}
	if len(cfg.Tables[0].Name)+len("__new_empty") != 64 {
		t.Fatal("用例构造有误")
	}
	// 54 字符表名 + 11 = 65，超限，启动即报错。
	name54 := strings.Repeat("b", 54)
	_, err = LoadConfig(writeTempConfig(t, base+name54+"\n    mode: full_table\n    archive_table: "+name54+"_archive\n"))
	if err == nil || !strings.Contains(err.Error(), "64") {
		t.Fatalf("超长表名应在校验阶段被拒绝并说明上限，实际 %v", err)
	}
	// 条件模式不建临时空表，不受此限制。
	_, err = LoadConfig(writeTempConfig(t, base+strings.Repeat("c", 60)+"\n    mode: conditional\n    archive_table: c_archive\n    condition: 'id>0'\n"))
	if err != nil {
		t.Fatalf("条件模式不建临时表，不应受限: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{"缺少 DSN", "tables:\n  - name: a\n    mode: full_table\n    archive_table: a_h\n", "dsn"},
		{"没有表", "database:\n  dsn: x\n", "至少"},
		{"非法表名", "database:\n  dsn: x\ntables:\n  - name: 'a-b'\n    mode: full_table\n    archive_table: a_h\n", "标识符"},
		{"同名表", "database:\n  dsn: x\ntables:\n  - name: a\n    mode: full_table\n    archive_table: a\n", "不能同名"},
		{"全表带条件", "database:\n  dsn: x\ntables:\n  - name: a\n    mode: full_table\n    archive_table: ah\n    condition: 'id>1'\n", "不允许配置 condition"},
		{"条件模式无条件", "database:\n  dsn: x\ntables:\n  - name: a\n    mode: conditional\n    archive_table: ah\n", "必须配置 condition"},
		{"条件含分号", "database:\n  dsn: x\ntables:\n  - name: a\n    mode: conditional\n    archive_table: ah\n    condition: 'id>1; drop t'\n", "分号"},
		{"条件含 UNION", "database:\n  dsn: x\ntables:\n  - name: a\n    mode: conditional\n    archive_table: ah\n    condition: 'id in (select 1 union select 2)'\n", "UNION"},
		{"非法数值字段", "database:\n  dsn: x\ntables:\n  - name: a\n    mode: full_table\n    archive_table: ah\n    verify:\n      numeric_fields: ['a b']\n", "列名"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeTempConfig(t, tc.content))
			if err == nil {
				t.Fatalf("期望报错，实际成功")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息 %q 不含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}
