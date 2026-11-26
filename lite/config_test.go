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
	if tbl.Verify.AbsTolerance != 0.0001 || tbl.Verify.RelTolerance != 0.00001 {
		t.Fatalf("容差默认值不对: %+v", tbl.Verify)
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
