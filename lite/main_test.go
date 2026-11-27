package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestProcessTableSkipNotApplicable(t *testing.T) {
	// 全表模式跑 verify：必须在碰数据库之前就返回"不适用"，且可被 errors.Is 识别。
	err := processTable(context.Background(), nil, &Config{},
		&TableCfg{Name: "logs", Mode: ModeFullTable, ArchiveTable: "logs_archive"}, "verify", 1, false)
	if !errors.Is(err, errActionNotApplicable) {
		t.Fatalf("应返回不适用错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("提示应指引改用 migrate: %v", err)
	}
	// 条件模式的校验动作不在这个分支，不能误判成"不适用"。
	err = processTable(context.Background(), nil, &Config{},
		&TableCfg{Name: "orders", Mode: "unknown_mode"}, "verify", 1, false)
	if err == nil || errors.Is(err, errActionNotApplicable) {
		t.Fatalf("未知模式应报配置错误而非跳过，实际 %v", err)
	}
}

func TestSampleSeedNotice(t *testing.T) {
	notice := SampleSeedNotice("trans_label", 1700000000123456789)
	want := "表 trans_label 本次抽样种子: 1700000000123456789（加 -seed 1700000000123456789 可复现本轮抽样）"
	if notice != want {
		t.Fatalf("提示行=%q，期望 %q", notice, want)
	}
	other := SampleSeedNotice("trans_label", 42)
	if other == notice {
		t.Fatal("不同种子应生成不同提示行")
	}
}

func TestResolveSampleSeed(t *testing.T) {
	cases := []struct {
		name            string
		requested, auto int64
		want            int64
	}{
		{"未指定用自动种子", -1, 1700000000123456789, 1700000000123456789},
		{"指定种子原样返回", 12345, 1700000000123456789, 12345},
		{"指定零种子可复现", 0, 1700000000123456789, 0},
		{"其他负数也视为自动", -99, 42, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveSampleSeed(tc.requested, tc.auto); got != tc.want {
				t.Fatalf("ResolveSampleSeed(%d,%d)=%d，期望 %d", tc.requested, tc.auto, got, tc.want)
			}
		})
	}
}
