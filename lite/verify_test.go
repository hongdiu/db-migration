package main

import (
	"strings"
	"testing"
)

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
	if r := JudgeCounts("分块计数", 100, 100); !r.Pass {
		t.Fatalf("相等应通过: %+v", r)
	}
	if r := JudgeCounts("分块计数", 100, 98); r.Pass {
		t.Fatal("不等应失败")
	}
}

func TestJudgeNonNullCounts(t *testing.T) {
	// 正常相等。
	r := JudgeNonNullCounts("amount 非空数", TextValue("100"), TextValue("100"))
	if !r.Pass || !strings.Contains(r.Detail, "100") {
		t.Fatalf("非空数相等应通过: %+v", r)
	}
	// 一侧 50 行 NULL、另一侧没有 NULL：SUM 可能相等，非空数必须判失败。
	r = JudgeNonNullCounts("amount 非空数", TextValue("100"), TextValue("50"))
	if r.Pass {
		t.Fatal("非空行数不等应判失败")
	}
	if !strings.Contains(r.Detail, "NULL 不算 0") {
		t.Fatalf("失败说明应点明 NULL 语义: %s", r.Detail)
	}
	// COUNT(col) 不会返回 NULL；万一传进来要报异常而不是误判通过。
	r = JudgeNonNullCounts("amount 非空数", NullValue(), TextValue("0"))
	if r.Pass || !strings.Contains(r.Detail, "异常") {
		t.Fatalf("NULL 输入应判异常: %+v", r)
	}
}

func TestAllPassed(t *testing.T) {
	results := []CheckResult{
		{Label: "记录数", Pass: true},
		{Label: "分块[0,99]计数", Pass: true},
		{Label: "分块[100,199]计数", Pass: false},
	}
	if AllPassed(results) {
		t.Fatal("有失败项时不应全部通过")
	}
	// 全部通过时判定为真。
	results[2].Pass = true
	if !AllPassed(results) {
		t.Fatal("全部通过判定失败")
	}
	// 空结果视为通过（没有可比对项），与调用方语义一致。
	if !AllPassed(nil) {
		t.Fatal("空结果应视为通过")
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
