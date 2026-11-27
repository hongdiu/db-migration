package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildReviewSQLByType(t *testing.T) {
	fields := []ReviewField{
		{Name: "amount", Kind: NumericDecimal},
		{Name: "fee", Kind: NumericInteger},
		{Name: "rate", Kind: NumericFloat},
		{Name: "note", Kind: NumericNone},
	}
	results := []ReviewItemResult{
		{Label: "记录数", Verdict: "通过", Detail: "两侧均为 1000 行"},
	}
	sql := BuildReviewSQL("orders", "orders_archive", "created_at < '2024-01-01'", "id", fields, results, nil, 12345, 0.0001, 0.00001)

	// 整数列：零容差字样。
	if !strings.Contains(sql, "字段 amount [定点小数] —— 零容差精确判定") {
		t.Fatal("定点小数列应标注零容差精确判定")
	}
	if !strings.Contains(sql, "字段 fee [整数] —— 零容差精确判定") {
		t.Fatal("整数列应标注零容差精确判定")
	}
	// 浮点列：必须生成容差判定 SQL，且带具体阈值和 SET 变量。
	if !strings.Contains(sql, "字段 rate [浮点] —— 容差判定") {
		t.Fatal("浮点列应标注容差判定")
	}
	if !strings.Contains(sql, "SET @src_sum") || !strings.Contains(sql, "SET @dst_sum") {
		t.Fatal("浮点列应生成可直接运行的容差判定 SQL")
	}
	if !strings.Contains(sql, "0.0001 + 1e-05") {
		t.Fatal("浮点容差 SQL 应带上具体阈值")
	}
	// 非数值列：不生成求和。
	if !strings.Contains(sql, "字段 note [非数值]") {
		t.Fatal("非数值列应显式说明不参与求和")
	}
	// 非空数小节存在。
	if !strings.Contains(sql, "核对项 3：非空数量") {
		t.Fatal("应包含非空数量核对，防 NULL 与 0 混淆")
	}
	// 工具判定汇总存在。
	if !strings.Contains(sql, "[通过] 记录数") {
		t.Fatal("应包含工具判定汇总")
	}
	// 源表带条件，归档表不带条件。
	if !strings.Contains(sql, "FROM `orders` WHERE created_at < '2024-01-01'") {
		t.Fatal("源表求和应带迁移条件")
	}
}

func TestBuildReviewSQLFullTable(t *testing.T) {
	// 全表模式没有条件，两侧都不应带 WHERE。
	sql := BuildReviewSQL("logs", "logs_archive", "", "id", nil, nil, nil, 12345, 0.0001, 0.00001)
	if strings.Contains(sql, " WHERE ") {
		t.Fatalf("全表模式复查 SQL 不应有 WHERE: %s", sql)
	}
	if !strings.Contains(sql, "SELECT COUNT(*) AS 源表行数 FROM `logs`;") {
		t.Fatal("应有源表计数 SQL")
	}
}

func TestBuildReviewSQLSampleSection(t *testing.T) {
	rounds := [][]int64{{1, 3, 5}, {7, 9}}
	sql := BuildReviewSQL("orders", "orders_archive", "created_at < '2024-01-01'", "id", nil, nil, rounds, 12345, 0.0001, 0.00001)
	if !strings.Contains(sql, "核对项 4：随机抽样逐字段比对") {
		t.Fatal("应包含抽样小节")
	}
	if !strings.Contains(sql, "第 1 轮抽中 3 行") || !strings.Contains(sql, "第 2 轮抽中 2 行") {
		t.Fatal("应逐轮列出抽中行数")
	}
	if !strings.Contains(sql, "WHERE `id` IN (1,3,5)") || !strings.Contains(sql, "WHERE `id` IN (7,9)") {
		t.Fatal("应输出按真实主键回读两表的 SQL")
	}
	if strings.Count(sql, "-- 源表") < 2 || strings.Count(sql, "-- 归档表") < 2 {
		t.Fatal("每轮都应有源表和归档表的回读 SQL")
	}
	// 未开启抽样时不应有抽样小节。
	sql2 := BuildReviewSQL("orders", "orders_archive", "", "id", nil, nil, nil, 12345, 0.0001, 0.00001)
	if strings.Contains(sql2, "核对项 4") {
		t.Fatal("未开启抽样不应输出抽样小节")
	}
}

func TestReviewSQLRecordsSampleSeed(t *testing.T) {
	rounds := [][]int64{{1, 3, 5}}
	sql := BuildReviewSQL("orders", "orders_archive", "", "id", nil, nil, rounds, 987654, 0.0001, 0.00001)
	if !strings.Contains(sql, "抽样种子: 987654") || !strings.Contains(sql, "-seed 987654") {
		t.Fatalf("有抽样时应标注种子及复现参数: %s", sql)
	}
	// 未开启抽样时不写种子行。
	sql2 := BuildReviewSQL("orders", "orders_archive", "", "id", nil, nil, nil, 987654, 0.0001, 0.00001)
	if strings.Contains(sql2, "抽样种子") {
		t.Fatal("未开启抽样不应标注种子")
	}
}

func TestBuildSampleReviewSQL(t *testing.T) {
	rounds := [][]int64{{11, 22}, {33}}
	results := []ReviewItemResult{{Label: "抽样第1轮逐字段比对（2行）", Verdict: "通过", Detail: "2 行全部字段一致"}}
	sql := BuildSampleReviewSQL("orders", "orders_archive", "created_at < '2024-01-01'", "id", results, rounds, 4242, 0.0001, 0.00001)
	for _, want := range []string{"独立抽样核对复查 SQL", "抽样种子: 4242", "[通过] 抽样第1轮", "第 1 轮抽中 2 行", "WHERE `id` IN (11,22)", "WHERE `id` IN (33)"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("抽样复查 SQL 缺少 %q: %s", want, sql)
		}
	}
	// 只含抽样，不夹带记录数/求和小节。
	if strings.Contains(sql, "核对项 1：记录数") || strings.Contains(sql, "核对项 2：数值列求和") {
		t.Fatal("独立抽样复查 SQL 不应夹带记录数/求和项")
	}
}

func TestOutputSampleSQLNameHasSeed(t *testing.T) {
	dir := t.TempDir()
	path, err := OutputSampleSQL(dir, "orders", 4242, "SELECT 1;\n")
	if err != nil {
		t.Fatalf("输出失败: %v", err)
	}
	if !strings.Contains(filepath.Base(path), "seed4242") {
		t.Fatalf("文件名应带种子: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("文件未生成: %v", err)
	}
}

func TestOutputReviewSQLWritesFile(t *testing.T) {
	dir := t.TempDir()
	path, err := OutputReviewSQL(dir, "orders", "SELECT 1;\n")
	if err != nil {
		t.Fatalf("输出失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("文件未生成: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if strings.TrimSpace(string(data)) != "SELECT 1;" {
		t.Fatalf("文件内容与传入 SQL 不一致: %q", string(data))
	}
	if filepath.Ext(path) != ".sql" {
		t.Fatalf("扩展名应为 .sql: %s", path)
	}
}
