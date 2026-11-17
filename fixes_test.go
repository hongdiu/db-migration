package main

import (
	"os"
	"strings"
	"testing"
)

// TestPurgeCountVerdict 是 P0-2 的回归测试。
// 清理阶段一旦中断，源表已少了一部分记录，若仍要求“源表剩余数 == 历史表数”，
// 续跑永远失败，断点续跑形同虚设。
func TestPurgeCountVerdict(t *testing.T) {
	cases := []struct {
		name                  string
		src, hist, deleted    int64
		singleBatch, resuming bool
		want                  bool
	}{
		{"首次执行严格相等", 1000, 1000, 0, true, false, true},
		{"首次执行有未归档新数据", 1005, 1000, 0, true, false, false},
		{"首次执行历史表多出记录", 900, 1000, 0, true, false, false},
		{"续跑: 剩余+已删=历史表", 600, 1000, 400, true, true, true},
		{"续跑: 源表超出总量", 700, 1000, 400, true, true, false},
		{"续跑: 部分记录被业务删除", 550, 1000, 400, true, true, true},
		{"多批次: 剩余+已删不超过总量", 800, 1000, 150, false, true, true},
		{"多批次: 超出总量", 900, 1000, 150, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := purgeCountVerdict(c.src, c.hist, c.deleted, c.singleBatch, c.resuming)
			if got != c.want {
				t.Fatalf("verdict=%v want %v (reason=%s)", got, c.want, reason)
			}
			if !got && reason == "" {
				t.Error("判定失败时应给出原因")
			}
		})
	}
}

// TestBaselineRoundTrip 是 P0-4 的回归测试。
// 全表迁移后源表为空，只能靠迁移前落盘的基线复核，因此基线必须能完整还原。
func TestBaselineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tc := &TableConfig{Name: "orders", Mode: "full_table", HistorySuffix: "_history", KeyColumn: "id"}
	tc.HistoryTimestamp = boolPtr(true)

	amount, err := ParseExactNumber("12345678901234567890.12", NumericDecimal)
	if err != nil {
		t.Fatal(err)
	}
	fee, err := ParseExactNumber("0.1", NumericFloat)
	if err != nil {
		t.Fatal(err)
	}
	minNum, _ := ParseExactNumber("1.00", NumericDecimal)
	maxNum, _ := ParseExactNumber("999.99", NumericDecimal)

	b := &baseline{
		count:    42,
		sums:     map[string]*ExactNumber{"amount": amount, "fee": fee},
		nonempty: map[string]int64{"amount": 42},
		minmax: map[string]*MinMaxValue{
			"amount": {Kind: NumericDecimal, MinText: "1.00", MaxText: "999.99", MinNum: minNum, MaxNum: maxNum},
		},
		snaps: [][]*RowSnapshot{{
			{Key: 7, Values: map[string]string{"id": "7", "amount": "1.00", "note": nullMarker}, Kinds: map[string]NumericKind{"id": NumericInteger, "amount": NumericDecimal, "note": NumericNone}},
		}},
		kinds: map[string]NumericKind{"id": NumericInteger, "amount": NumericDecimal, "note": NumericNone},
	}

	path, err := saveBaseline(dir, tc, b)
	if err != nil {
		t.Fatalf("保存基线失败: %v", err)
	}
	got, err := loadBaseline(path)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}
	if got.count != 42 {
		t.Errorf("记录数还原错误: %d", got.count)
	}
	// 关键：超大定点小数必须精确还原，不能经过 float64。
	if got.sums["amount"].Raw != "12345678901234567890.12" {
		t.Errorf("求和值还原错误: %s", got.sums["amount"].Raw)
	}
	if got.sums["amount"].Rat.Cmp(amount.Rat) != 0 {
		t.Error("求和值精度丢失")
	}
	if got.nonempty["amount"] != 42 {
		t.Error("非空数还原错误")
	}
	if got.minmax["amount"] == nil || got.minmax["amount"].MinNum == nil {
		t.Fatal("最值未还原")
	}
	if got.minmax["amount"].MinNum.Rat.Cmp(minNum.Rat) != 0 {
		t.Error("最小值精度丢失")
	}
	if len(got.snaps) != 1 || len(got.snaps[0]) != 1 {
		t.Fatal("抽样快照未还原")
	}
	if got.snaps[0][0].Values["note"] != nullMarker {
		t.Error("NULL 标记未还原")
	}
	if got.snaps[0][0].Kinds["amount"] != NumericDecimal {
		t.Error("字段数值类别未还原")
	}
}

func TestLoadBaselineMissing(t *testing.T) {
	if _, err := loadBaseline(t.TempDir() + "/nope.json"); err == nil {
		t.Error("缺失的基线文件应返回错误")
	}
}

// TestValidateVerifyFields 是 P1-6 的回归测试。
// 求和字段配成非数值列时必须直接报错：数据库会对非数值列 SUM 出 0，
// 静默“通过”核对，属于漏检。
func TestValidateVerifyFields(t *testing.T) {
	cols := []ColumnMeta{
		{Name: "id", DataType: "bigint", Kind: NumericInteger},
		{Name: "amount", DataType: "decimal", Kind: NumericDecimal},
		{Name: "note", DataType: "varchar", Kind: NumericNone},
	}

	ok := &VerifyConfig{NumericSumFields: []string{"amount", "id"}}
	if err := validateVerifyFields(cols, ok); err != nil {
		t.Errorf("数值字段求和不应报错: %v", err)
	}

	bad := &VerifyConfig{NumericSumFields: []string{"note"}}
	if err := validateVerifyFields(cols, bad); err == nil {
		t.Error("非数值字段求和应当报错")
	}

	missing := &VerifyConfig{NumericSumFields: []string{"not_exist"}}
	if err := validateVerifyFields(cols, missing); err == nil {
		t.Error("不存在的字段应当报错")
	}
}

// TestWriteDetailFilesOnSummaryFailure 是 P1-9 的回归测试。
// 记录数、求和这类汇总项失败但没有任何字段差异时，旧实现不产出任何文件，
// 现场只剩一行控制台输出，无法继续分析。
func TestWriteDetailFilesOnSummaryFailure(t *testing.T) {
	dir := t.TempDir()
	rep := &TableReport{Table: "orders", Mode: "full_table", Stage: "rename+verify", OutputDir: dir}
	rep.addWithRule("记录数比对", false, "迁移前 10 / 迁移后 9", "记录数精确相等")
	rep.addWithRule("SUM(amount)", true, "精确相等", "定点小数零容差")

	if rep.Pass() {
		t.Fatal("存在失败项时不应判定为通过")
	}
	files, err := rep.WriteDetailFiles()
	if err != nil {
		t.Fatalf("写结果文件失败: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("汇总项失败时也必须产出结果文件")
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "记录数比对") || !strings.Contains(content, "失败") {
		t.Errorf("结果文件未包含失败核对项: %s", content)
	}
}

func TestWriteDetailFilesSkipOnPass(t *testing.T) {
	dir := t.TempDir()
	rep := &TableReport{Table: "orders", Mode: "full_table", OutputDir: dir}
	rep.addWithRule("记录数比对", true, "10 / 10", "")
	files, err := rep.WriteDetailFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("全部通过时不应产出结果文件: %v", files)
	}
}
