package main

import "testing"

func TestMakeBlocks(t *testing.T) {
	blocks := MakeBlocks(1, 10, 3)
	if len(blocks) != 4 {
		t.Fatalf("1~10 按 3 分块应为 4 块，实际 %d: %+v", len(blocks), blocks)
	}
	if blocks[0] != (BlockRange{1, 3}) || blocks[3] != (BlockRange{10, 10}) {
		t.Fatalf("分块区间不对: %+v", blocks)
	}
	// 空表。
	if got := MakeBlocks(5, 1, 100); got != nil {
		t.Fatalf("空表应返回 nil，实际 %+v", got)
	}
	// 单块。
	if got := MakeBlocks(1, 100, 500); len(got) != 1 || got[0] != (BlockRange{1, 100}) {
		t.Fatalf("单块不对: %+v", got)
	}
	// 上界溢出保护：大数不应无限循环或回绕。
	big := MakeBlocks(9223372036854775800, 9223372036854775807, 4)
	if len(big) == 0 || big[len(big)-1].Hi != 9223372036854775807 {
		t.Fatalf("溢出保护失败: %+v", big)
	}
}

func TestFitBlockSize(t *testing.T) {
	// 常规跨度、块数没超限：保持原块宽。
	if got := FitBlockSize(1, 1000000, 100000, 1000); got != 100000 {
		t.Fatalf("未超上限应保持原块宽，实际 %d", got)
	}
	// 雪花 ID 跨度 1e18：必须放大块宽，把块数压到上限内。
	const snowSpan = int64(1_000_000_000_000_000_000)
	bs := FitBlockSize(1, snowSpan, 100000, 1000)
	if bs == 100000 {
		t.Fatal("超大跨度应放大块宽")
	}
	blocks := MakeBlocks(1, snowSpan, bs)
	if len(blocks) > 1000 {
		t.Fatalf("分块数应不超过上限 1000，实际 %d（块宽 %d）", len(blocks), bs)
	}
	if len(blocks) == 0 || blocks[len(blocks)-1].Hi != snowSpan {
		t.Fatalf("分块应覆盖到最大主键: %+v", blocks[len(blocks)-1])
	}
	// 极值区间：不能溢出、不能返回非正块宽。
	extreme := FitBlockSize(-1<<63, 1<<63-1, 100000, 1000)
	if extreme <= 0 {
		t.Fatalf("极值区间块宽应为正数，实际 %d", extreme)
	}
	// 非法输入原样返回，由调用方按默认值处理。
	if got := FitBlockSize(10, 5, 100, 10); got != 100 {
		t.Fatalf("空区间应返回原块宽，实际 %d", got)
	}
}

func TestJudgeCounts(t *testing.T) {
	block := BlockRange{Lo: 0, Hi: 99}
	if r := JudgeCounts("分块计数", 100, 100, block); !r.Pass {
		t.Fatalf("相等应通过: %+v", r)
	}
	r := JudgeCounts("分块计数", 100, 98, block)
	if r.Pass {
		t.Fatal("不等应失败")
	}
	if r.BlockLo != 0 || r.BlockHi != 99 {
		t.Fatalf("分块信息应保留: %+v", r)
	}
}

func TestJudgeNumericAggregateByType(t *testing.T) {
	// 整数：差 1 失败。
	if r := JudgeNumericAggregate("求和", "100", "101", NumericInteger, 0.0001, 0.00001, false); r.Pass {
		t.Fatal("整数差 1 应失败")
	}
	// 定点小数：写法差异通过。
	if r := JudgeNumericAggregate("求和", "1.0", "1.00", NumericDecimal, 0.0001, 0.00001, false); !r.Pass {
		t.Fatal("定点小数写法差异应通过")
	}
	// 浮点：容差内通过。
	if r := JudgeNumericAggregate("求和", "1.00005", "1.0", NumericFloat, 0.0001, 0.00001, false); !r.Pass {
		t.Fatal("浮点容差内应通过")
	}
	// 浮点：超容差失败。
	if r := JudgeNumericAggregate("求和", "1.001", "1.0", NumericFloat, 0.0001, 0.00001, false); r.Pass {
		t.Fatal("浮点超容差应失败")
	}
	// NULL 与 0 不等。
	if r := JudgeNumericAggregate("求和", "", "0", NumericDecimal, 0.0001, 0.00001, true); r.Pass {
		t.Fatal("NULL 与 0 不应相等")
	}
}

func TestAllPassedAndFailedBlocks(t *testing.T) {
	results := []CheckResult{
		{Label: "记录数", Pass: true},
		{Label: "块1", Pass: true, BlockLo: 0, BlockHi: 99},
		{Label: "块2", Pass: false, BlockLo: 100, BlockHi: 199},
	}
	if AllPassed(results) {
		t.Fatal("有失败项时不应全部通过")
	}
	failed := FailedBlocks(results)
	if len(failed) != 1 || failed[0] != (BlockRange{100, 199}) {
		t.Fatalf("失败分块提取不对: %+v", failed)
	}
	// 全部通过时 FailedBlocks 为空。
	results[2].Pass = true
	if !AllPassed(results) {
		t.Fatal("全部通过判定失败")
	}
	if got := FailedBlocks(results); len(got) != 0 {
		t.Fatalf("全通过时失败分块应为空: %+v", got)
	}
}

func TestResultsToReviewItems(t *testing.T) {
	results := []CheckResult{
		{Label: "记录数", Pass: true, Detail: "两侧 100 行"},
		{Label: "求和", Pass: false, Detail: "差 1"},
	}
	items := ResultsToReviewItems(results)
	if len(items) != 2 {
		t.Fatalf("项数不对: %d", len(items))
	}
	if items[0].Verdict != "通过" || items[1].Verdict != "不通过" {
		t.Fatalf("判定文案不对: %+v", items)
	}
}
