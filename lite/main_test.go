package main

import "testing"

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
		name               string
		requested, auto    int64
		want               int64
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
