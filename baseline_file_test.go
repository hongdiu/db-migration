package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleColumns() []ColumnMeta {
	return []ColumnMeta{
		{Name: "id", DataType: "bigint", Kind: NumericInteger},
		{Name: "amount", DataType: "decimal", Kind: NumericDecimal},
		{Name: "note", DataType: "varchar", Kind: NumericNone},
	}
}

// TestComputeSchemaHash 校验结构指纹对列名、类型、顺序敏感，对大小写不敏感。
func TestComputeSchemaHash(t *testing.T) {
	base := computeSchemaHash(sampleColumns())
	if base == "" {
		t.Fatal("结构指纹不应为空")
	}
	// 大小写变化不应改变指纹（数据库返回大小写不统一是常态）。
	upper := []ColumnMeta{
		{Name: "ID", DataType: "BIGINT", Kind: NumericInteger},
		{Name: "AMOUNT", DataType: "DECIMAL", Kind: NumericDecimal},
		{Name: "NOTE", DataType: "VARCHAR", Kind: NumericNone},
	}
	if computeSchemaHash(upper) != base {
		t.Error("列名/类型大小写不应影响结构指纹")
	}
	// 加列必须改变指纹。
	added := append(sampleColumns(), ColumnMeta{Name: "extra", DataType: "int", Kind: NumericInteger})
	if computeSchemaHash(added) == base {
		t.Error("新增列应改变结构指纹")
	}
	// 改类型必须改变指纹。
	changed := sampleColumns()
	changed[1].DataType = "double"
	if computeSchemaHash(changed) == base {
		t.Error("字段类型变化应改变结构指纹")
	}
	// 顺序变化必须改变指纹（列顺序影响 SELECT * 的解析）。
	reordered := []ColumnMeta{sampleColumns()[1], sampleColumns()[0], sampleColumns()[2]}
	if computeSchemaHash(reordered) == base {
		t.Error("列顺序变化应改变结构指纹")
	}
}

// TestBaselineSchemaHashRoundTrip 是 P2-3 的核心回归：基线必须带上结构指纹与采集时间，
// 否则事后复核无法判断“表结构是否变过”。
func TestBaselineSchemaHashRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tc := &TableConfig{Name: "orders", Mode: "full_table", HistorySuffix: "_history", KeyColumn: "id"}
	b := &baseline{
		count:      3,
		schemaHash: computeSchemaHash(sampleColumns()),
		capturedAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
	}
	path, err := saveBaseline(dir, tc, b)
	if err != nil {
		t.Fatalf("保存基线失败: %v", err)
	}
	got, err := loadBaseline(path)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}
	if got.schemaHash != b.schemaHash {
		t.Errorf("结构指纹未还原: got %q want %q", got.schemaHash, b.schemaHash)
	}
	if got.capturedAt.IsZero() || !got.capturedAt.Equal(b.capturedAt) {
		t.Errorf("采集时间未还原: got %v want %v", got.capturedAt, b.capturedAt)
	}
	if got.sourcePath != path {
		t.Errorf("基线来源路径未记录: got %q want %q", got.sourcePath, path)
	}
}

// TestCheckBaselineCompatible 覆盖三种情形：旧格式、结构变更、结构一致。
func TestCheckBaselineCompatible(t *testing.T) {
	tc := &TableConfig{Name: "orders"}
	cols := sampleColumns()

	// 旧版本基线没有结构指纹，必须拒绝，不能默默继续。
	legacy := &baseline{sourcePath: filepath.Join("x", "old.json")}
	if err := checkBaselineCompatible(legacy, tc, cols); err == nil {
		t.Error("缺少结构指纹的旧基线应报错")
	} else if !strings.Contains(err.Error(), "重新执行") {
		t.Errorf("报错应给出补救办法，实际: %v", err)
	}

	// 结构变更后必须拒绝，并同时暴露两个指纹便于排查。
	drifted := &baseline{schemaHash: "deadbeefdeadbeef", sourcePath: "p"}
	err := checkBaselineCompatible(drifted, tc, cols)
	if err == nil {
		t.Fatal("结构指纹不一致应报错")
	}
	if !strings.Contains(err.Error(), computeSchemaHash(cols)) {
		t.Errorf("报错应包含当前结构指纹，实际: %v", err)
	}

	// 结构一致时放行。
	ok := &baseline{schemaHash: computeSchemaHash(cols), sourcePath: "p"}
	if err := checkBaselineCompatible(ok, tc, cols); err != nil {
		t.Errorf("结构一致时不应报错: %v", err)
	}
}
