package main

import (
	"strings"
	"testing"
)

func TestCompareSampledRowIdentical(t *testing.T) {
	cols := []string{"id", "amount", "rate", "note"}
	kinds := map[string]NumericKind{
		"id": NumericInteger, "amount": NumericDecimal, "rate": NumericFloat, "note": NumericNone,
	}
	src := map[string]NumericValue{
		"id": TextValue("1"), "amount": TextValue("100.00"), "rate": TextValue("1.5"), "note": TextValue("hello"),
	}
	dst := map[string]NumericValue{
		"id": TextValue("1"), "amount": TextValue("100.0"), "rate": TextValue("1.5"), "note": TextValue("hello"),
	}
	if diffs := CompareSampledRow(1, cols, kinds, src, dst, testTolerances()); len(diffs) != 0 {
		t.Fatalf("完全一致的行的差异应为空: %+v", diffs)
	}
}

func TestCompareSampledRowCatchesSwap(t *testing.T) {
	// 核心场景：两行数据互换。求和/计数发现不了，但逐行比对必须抓到。
	cols := []string{"id", "amount", "note"}
	kinds := map[string]NumericKind{"id": NumericInteger, "amount": NumericDecimal, "note": NumericNone}
	// 源表 id=1 是 100/甲，归档表 id=1 却是 200/乙（两行内容互换）。
	src := map[string]NumericValue{
		"id": TextValue("1"), "amount": TextValue("100.00"), "note": TextValue("甲"),
	}
	dst := map[string]NumericValue{
		"id": TextValue("1"), "amount": TextValue("200.00"), "note": TextValue("乙"),
	}
	diffs := CompareSampledRow(1, cols, kinds, src, dst, testTolerances())
	if len(diffs) != 2 {
		t.Fatalf("互换场景应有 2 个字段差异（amount、note）: %+v", diffs)
	}
	got := make(map[string]bool)
	for _, d := range diffs {
		got[d.Column] = true
	}
	if !got["amount"] || !got["note"] {
		t.Fatalf("应抓到 amount 和 note 的差异: %+v", diffs)
	}
}

func TestCompareSampledRowNullVsZero(t *testing.T) {
	cols := []string{"optional_amt"}
	kinds := map[string]NumericKind{"optional_amt": NumericDecimal}
	src := map[string]NumericValue{"optional_amt": NullValue()}
	dst := map[string]NumericValue{"optional_amt": TextValue("0")}
	if diffs := CompareSampledRow(1, cols, kinds, src, dst, testTolerances()); len(diffs) != 1 {
		t.Fatalf("NULL 与 0 应判差异: %+v", diffs)
	}
}

func TestCompareSampledRowMissingColumn(t *testing.T) {
	cols := []string{"id", "amount"}
	kinds := map[string]NumericKind{"id": NumericInteger, "amount": NumericDecimal}
	src := map[string]NumericValue{"id": TextValue("1"), "amount": TextValue("100")}
	dst := map[string]NumericValue{"id": TextValue("1")} // 缺 amount
	diffs := CompareSampledRow(1, cols, kinds, src, dst, testTolerances())
	if len(diffs) != 1 || !strings.Contains(diffs[0].Reason, "缺失") {
		t.Fatalf("列缺失应报差异: %+v", diffs)
	}
}

func TestCompareSampledRowFloatTolerance(t *testing.T) {
	cols := []string{"rate"}
	kinds := map[string]NumericKind{"rate": NumericFloat}
	src := map[string]NumericValue{"rate": TextValue("1.00005")}
	dst := map[string]NumericValue{"rate": TextValue("1.0")}
	if diffs := CompareSampledRow(1, cols, kinds, src, dst, testTolerances()); len(diffs) != 0 {
		t.Fatalf("浮点容差内应通过: %+v", diffs)
	}
}

func TestReadRowKeyMatchesUnquotedColumns(t *testing.T) {
	// 回归测试：readRow 用带反引号的列名拼 SQL，但 map 的 key 必须是裸列名，
	// 否则 CompareSampledRow 用裸列名查找会全部报"列缺失"。
	// 这里验证：unquote 后的键名与 CompareSampledRow 的查找键一致。
	quoted := []string{"`id`", "`amount`", "`owner`"}
	bare := unquoteColumns(quoted)
	kinds := map[string]NumericKind{"id": NumericInteger, "amount": NumericDecimal, "owner": NumericNone}
	// 模拟 readRow 的键名构造方式（与 readRow 内 strings.Trim(col,"`") 一致）。
	row := make(map[string]NumericValue)
	for _, col := range quoted {
		row[strings.Trim(col, "`")] = TextValue("1")
	}
	diffs := CompareSampledRow(1, bare, kinds, row, row, testTolerances())
	if len(diffs) != 0 {
		t.Fatalf("键名一致时不应有差异: %+v", diffs)
	}
	// 对照：若键名带反引号（错误实现），应全部报列缺失。
	badRow := make(map[string]NumericValue)
	for _, col := range quoted {
		badRow[col] = TextValue("1")
	}
	badDiffs := CompareSampledRow(1, bare, kinds, badRow, badRow, testTolerances())
	if len(badDiffs) != 3 {
		t.Fatalf("键名不一致应报 3 处列缺失: %+v", badDiffs)
	}
}

func TestUnquoteColumns(t *testing.T) {
	got := unquoteColumns([]string{"`id`", "`amount`"})
	if len(got) != 2 || got[0] != "id" || got[1] != "amount" {
		t.Fatalf("去反引号不对: %+v", got)
	}
}

func TestDisplayValue(t *testing.T) {
	if displayValue(NullValue()) != "NULL" {
		t.Fatal("NULL 展示不对")
	}
	if displayValue(TextValue("abc")) != "abc" {
		t.Fatal("普通值展示不对")
	}
}
