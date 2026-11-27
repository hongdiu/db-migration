package main

import (
	"strings"
	"testing"
)

func TestJudgePrimaryKeyUsable(t *testing.T) {
	// 整型主键通过。
	key, err := judgePrimaryKeyUsable("", &PrimaryKeyInfo{Column: "id", DataType: "bigint"})
	if err != nil || key != "id" {
		t.Fatalf("整型主键应通过: key=%s err=%v", key, err)
	}
	// 配置与探测一致通过。
	if _, err := judgePrimaryKeyUsable("ID", &PrimaryKeyInfo{Column: "id", DataType: "int"}); err != nil {
		t.Fatalf("大小写不一致应视为相同: %v", err)
	}
	// 配置与探测不一致拒绝。
	if _, err := judgePrimaryKeyUsable("uid", &PrimaryKeyInfo{Column: "id", DataType: "int"}); err == nil {
		t.Fatal("配置主键与实际主键不一致应拒绝")
	}
	// 非整型主键拒绝。
	if _, err := judgePrimaryKeyUsable("", &PrimaryKeyInfo{Column: "uid", DataType: "varchar"}); err == nil {
		t.Fatal("字符串主键应拒绝")
	}
	// 复合主键（nil 模拟未通过探测）拒绝。
	if _, err := judgePrimaryKeyUsable("", nil); err == nil {
		t.Fatal("无主键应拒绝")
	}
}

func TestJudgeDependenciesAllowed(t *testing.T) {
	if err := judgeDependenciesAllowed(nil); err != nil {
		t.Fatalf("无依赖应允许: %v", err)
	}
	deps := []DependencyInfo{
		{Kind: "foreign_key", Name: "order_items", Ref: "fk_order"},
		{Kind: "view", Name: "v_orders", Ref: "..."},
	}
	err := judgeDependenciesAllowed(deps)
	if err == nil {
		t.Fatal("有依赖应拒绝")
	}
	msg := err.Error()
	if !strings.Contains(msg, "foreign_key") || !strings.Contains(msg, "view") {
		t.Fatalf("错误信息应列出两类依赖: %s", msg)
	}
}

func TestCompareTableStructure(t *testing.T) {
	src := []ColumnMeta{
		{Name: "id", DataType: "bigint", ColumnType: "bigint unsigned"},
		{Name: "amount", DataType: "decimal", ColumnType: "decimal(18,2)"},
	}
	if err := CompareTableStructure("orders", "orders_archive", src, src, nil, nil); err != nil {
		t.Fatalf("完全一致应通过: %v", err)
	}
	// 列类型只差长度：必须拦住，这类差异写入时会被静默截断。
	short := []ColumnMeta{
		{Name: "id", DataType: "bigint", ColumnType: "bigint unsigned"},
		{Name: "amount", DataType: "decimal", ColumnType: "decimal(10,2)"},
	}
	if err := CompareTableStructure("orders", "orders_archive", src, short, nil, nil); err == nil {
		t.Fatal("列精度不一致应拒绝")
	} else if !strings.Contains(err.Error(), "decimal(10,2)") {
		t.Fatalf("错误信息应指出具体类型差异: %v", err)
	}
	// 列数不一致。
	if err := CompareTableStructure("orders", "orders_archive", src, src[:1], nil, nil); err == nil {
		t.Fatal("列数不一致应拒绝")
	}
	// 列顺序不同：INSERT 按列名写入本身不受影响，但说明不是源表副本，按结构不符处理。
	swapped := []ColumnMeta{src[1], src[0]}
	if err := CompareTableStructure("orders", "orders_archive", src, swapped, nil, nil); err == nil {
		t.Fatal("列顺序不一致应拒绝")
	}
	// 只有 DataType（没有 ColumnType）时退化为按基础类型比对。
	plain := []ColumnMeta{{Name: "id", DataType: "bigint"}, {Name: "amount", DataType: "decimal"}}
	plainOther := []ColumnMeta{{Name: "id", DataType: "bigint"}, {Name: "amount", DataType: "int"}}
	if err := CompareTableStructure("a", "ah", plain, plainOther, nil, nil); err == nil {
		t.Fatal("基础类型不一致应拒绝")
	}
}

func TestCompareTableStructureIndexes(t *testing.T) {
	cols := []ColumnMeta{{Name: "id"}, {Name: "order_no"}}
	// 索引名不同但结构相同：跨表索引名可以不同，不应误报。
	srcIdx := []IndexMeta{{Name: "PRIMARY", Unique: true, Columns: []string{"id"}},
		{Name: "uk_order", Unique: true, Columns: []string{"order_no"}}}
	dstIdx := []IndexMeta{{Name: "PRIMARY", Unique: true, Columns: []string{"id"}},
		{Name: "uk_no", Unique: true, Columns: []string{"order_no"}}}
	if err := CompareTableStructure("a", "ah", cols, cols, srcIdx, dstIdx); err != nil {
		t.Fatalf("索引结构一致时不应因名字不同报错: %v", err)
	}
	// 归档表缺唯一索引：写入不再按该唯一键去重，必须拦住。
	if err := CompareTableStructure("a", "ah", cols, cols, srcIdx, dstIdx[:1]); err == nil {
		t.Fatal("缺少唯一索引应拒绝")
	} else if !strings.Contains(err.Error(), "索引不一致") {
		t.Fatalf("错误信息应点明索引差异: %v", err)
	}
	// 唯一性不同（唯一 vs 普通）也要拦。
	nonUnique := []IndexMeta{{Name: "PRIMARY", Unique: true, Columns: []string{"id"}},
		{Name: "uk_no", Unique: false, Columns: []string{"order_no"}}}
	if err := CompareTableStructure("a", "ah", cols, cols, srcIdx, nonUnique); err == nil {
		t.Fatal("唯一性不同应拒绝")
	}
}

func TestUpsertConflictIndexes(t *testing.T) {
	primary := IndexMeta{Name: "PRIMARY", Unique: true, Columns: []string{"id"}}
	// 只有主键：ON DUPLICATE KEY UPDATE 只会命中主键，安全。
	if got := upsertConflictIndexes([]IndexMeta{primary}, "id"); len(got) != 0 {
		t.Fatalf("只有主键时不应报冲突: %+v", got)
	}
	// 非主键唯一索引：必须列出。
	ukOrder := IndexMeta{Name: "uk_order", Unique: true, Columns: []string{"order_no"}}
	got := upsertConflictIndexes([]IndexMeta{primary, ukOrder}, "id")
	if len(got) != 1 || got[0].Name != "uk_order" {
		t.Fatalf("应识别出 uk_order 冲突: %+v", got)
	}
	// 复合唯一索引同样会触发整行覆盖。
	ukPair := IndexMeta{Name: "uk_pair", Unique: true, Columns: []string{"merchant_id", "biz_no"}}
	if got := upsertConflictIndexes([]IndexMeta{primary, ukPair}, "id"); len(got) != 1 {
		t.Fatalf("复合唯一索引应报冲突: %+v", got)
	}
	// 与主键同列的唯一索引不引入新冲突面（命中它必然也命中主键），大小写与顺序无关。
	sameKey := IndexMeta{Name: "uk_id", Unique: true, Columns: []string{"ID"}}
	if got := upsertConflictIndexes([]IndexMeta{primary, sameKey}, "id"); len(got) != 0 {
		t.Fatalf("与主键同列的唯一索引不应误报: %+v", got)
	}
	// 普通索引不参与唯一性判定。
	plain := IndexMeta{Name: "idx_order", Unique: false, Columns: []string{"order_no"}}
	if got := upsertConflictIndexes([]IndexMeta{primary, plain}, "id"); len(got) != 0 {
		t.Fatalf("普通索引不应报冲突: %+v", got)
	}
	// 没有 PRIMARY 记录时按 keyCol 兜底，仍能识别出"同列唯一索引"。
	if got := upsertConflictIndexes([]IndexMeta{sameKey}, "id"); len(got) != 0 {
		t.Fatalf("无 PRIMARY 记录时应按 keyCol 兜底: %+v", got)
	}
}

func TestJudgeUpsertUniqueIndexes(t *testing.T) {
	primary := IndexMeta{Name: "PRIMARY", Unique: true, Columns: []string{"id"}}
	if err := judgeUpsertUniqueIndexes("orders", []IndexMeta{primary}, "id"); err != nil {
		t.Fatalf("只有主键应放行: %v", err)
	}
	uk := IndexMeta{Name: "uk_order", Unique: true, Columns: []string{"order_no"}}
	err := judgeUpsertUniqueIndexes("orders", []IndexMeta{primary, uk}, "id")
	if err == nil {
		t.Fatal("存在非主键唯一索引应拒绝")
	}
	msg := err.Error()
	for _, want := range []string{"uk_order", "order_no", "ON DUPLICATE KEY UPDATE", "-force"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("提示信息应包含 %q，实际：%s", want, msg)
		}
	}
}

func TestJudgeArchiveTableFree(t *testing.T) {
	if err := judgeArchiveTableFree(false, "orders_archive"); err != nil {
		t.Fatalf("归档表不存在应允许: %v", err)
	}
	if err := judgeArchiveTableFree(true, "orders_archive"); err == nil {
		t.Fatal("归档表已存在应拒绝")
	}
}

func TestIsGeneratedColumn(t *testing.T) {
	if !isGeneratedColumn("VIRTUAL GENERATED") {
		t.Fatal("虚拟生成列应识别")
	}
	if !isGeneratedColumn("STORED GENERATED") {
		t.Fatal("存储生成列应识别")
	}
	if isGeneratedColumn("") || isGeneratedColumn("auto_increment") {
		t.Fatal("普通列不应误判为生成列")
	}
}

func TestQIdent(t *testing.T) {
	if qIdent("orders") != "`orders`" {
		t.Fatalf("反引号包裹失败: %s", qIdent("orders"))
	}
}
