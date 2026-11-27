// 复查 SQL 生成与输出。
//
// 用户会拿这份 SQL 贴到数据库客户端亲自核对，所以要求：
//   - 按字段类型分别生成，整数/定点小数是精确判定，浮点是容差判定，不能共用一套模板。
//   - 金额类字段附非空数，避免 NULL 被当成 0 造成"看起来对不上"。
//   - 每一项标注字段类型、判定标准、工具自己的结果。
//   - 同一份内容既写 .sql 文件，也原样打印到控制台。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReviewField 是参与复查的一个字段及其探测到的类型。
type ReviewField struct {
	Name string
	Kind NumericKind
}

// ReviewItemResult 是工具对某个核对项的判定结果，写进 SQL 注释供对照。
type ReviewItemResult struct {
	Label   string // 核对项名称
	Verdict string // 通过 / 不通过
	Detail  string // 判定说明
}

// sampleSeedNote 生成抽样种子说明行；未开启抽样时不输出。
// 记录种子是为了能用 -seed 复现本次抽中的行。
func sampleSeedNote(sampleRounds [][]int64, seed int64) string {
	if len(sampleRounds) == 0 {
		return ""
	}
	return fmt.Sprintf("-- 抽样种子: %d（加 -seed %d 可复现本轮抽样）\n", seed, seed)
}

// BuildReviewSQL 生成完整的复查 SQL 文本。
// sampleRounds 是逐轮抽中的主键清单；传 nil 表示未开启抽样，不输出抽样小节。
// tols 按字段类型提供 FLOAT / DOUBLE 各自的容差。
func BuildReviewSQL(sourceTable, archiveTable, condition, keyCol string, fields []ReviewField, results []ReviewItemResult, sampleRounds [][]int64, seed int64, tols Tolerances) string {
	return BuildReviewSQLWithArchiveCondition(sourceTable, archiveTable, condition, "", keyCol, fields, results, sampleRounds, seed, tols)
}

// BuildReviewSQLWithArchiveCondition 在附加归档表过滤条件下生成复查 SQL。
// 删除续跑时传入"主键 > 上次删除位置"，让人工复查看到的范围与工具校验一致。
func BuildReviewSQLWithArchiveCondition(sourceTable, archiveTable, condition, archiveCondition, keyCol string, fields []ReviewField, results []ReviewItemResult, sampleRounds [][]int64, seed int64, tols Tolerances) string {
	var sb strings.Builder
	sb.WriteString("-- 数据迁移复查 SQL\n")
	sb.WriteString(fmt.Sprintf("-- 生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("-- 源表: %s    归档表: %s\n", sourceTable, archiveTable))
	if condition != "" {
		sb.WriteString(fmt.Sprintf("-- 迁移条件: %s\n", condition))
	}
	sb.WriteString(sampleSeedNote(sampleRounds, seed))
	sb.WriteString("-- 说明: 每项都给出可直接执行的 SQL，并在注释里标注工具自己的判定，供人工对照。\n\n")

	// 工具判定汇总放最前面，便于先看结论再看 SQL。
	sb.WriteString("-- ========== 工具判定汇总 ==========\n")
	for _, r := range results {
		sb.WriteString(fmt.Sprintf("-- [%s] %s：%s\n", r.Verdict, r.Label, r.Detail))
	}
	sb.WriteString("\n")

	// 核对项一：记录数。
	sb.WriteString("-- ========== 核对项 1：记录数（零容差，两侧行数必须相等） ==========\n")
	srcWhere := ""
	if condition != "" {
		srcWhere = " WHERE " + condition
	}
	archiveWhere := sampleWhere(archiveCondition)
	if archiveWhere != "" {
		sb.WriteString(fmt.Sprintf("-- 归档表校验范围: %s（续跑时排除已删部分）\n", archiveCondition))
	}
	sb.WriteString(fmt.Sprintf("SELECT COUNT(*) AS 源表行数 FROM %s%s;\n", qIdent(sourceTable), srcWhere))
	sb.WriteString(fmt.Sprintf("SELECT COUNT(*) AS 归档表行数 FROM %s%s;\n\n", qIdent(archiveTable), archiveWhere))

	// 核对项二：按字段类型的求和比对。
	sb.WriteString("-- ========== 核对项 2：数值列求和（按字段类型分别判定） ==========\n")
	for _, f := range fields {
		sb.WriteString(buildFieldReview(f, sourceTable, archiveTable, srcWhere, archiveWhere, tols))
		sb.WriteString("\n")
	}

	// 核对项三：非空数，专门防 NULL 与 0 混淆。
	if len(fields) > 0 {
		sb.WriteString("-- ========== 核对项 3：非空数量（NULL 不算 0，两侧非空数必须相等） ==========\n")
		for _, f := range fields {
			sb.WriteString(fmt.Sprintf("-- 字段 %s [%s]\n", f.Name, f.Kind))
			sb.WriteString(fmt.Sprintf("SELECT COUNT(%s) AS 源表非空数 FROM %s%s;\n", qIdent(f.Name), qIdent(sourceTable), srcWhere))
			sb.WriteString(fmt.Sprintf("SELECT COUNT(%s) AS 归档表非空数 FROM %s%s;\n\n", qIdent(f.Name), qIdent(archiveTable), archiveWhere))
		}
	}

	// 核对项四：随机抽样逐字段比对。
	if len(sampleRounds) > 0 {
		sb.WriteString("-- ========== 核对项 4：随机抽样逐字段比对（抓行级错位，如两行数据互换） ==========\n")
		sb.WriteString("-- 抽样方式：主键分区随机取点（候选不足时降级为蓄水池抽样）；下面是抽中的主键，可回读两表所有可比对列（生成列除外）人工比对。\n")
		for r, keys := range sampleRounds {
			if len(keys) == 0 {
				continue
			}
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%d", k))
			}
			sb.WriteString(fmt.Sprintf("-- 第 %d 轮抽中 %d 行\n", r+1, len(keys)))
			// 给出按主键回读两表所有可比对列（生成列除外）的 SQL，用户可直接贴到客户端对照。
			sb.WriteString(fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s) ORDER BY %s;   -- 源表\n", qIdent(sourceTable), qIdent(keyCol), strings.Join(parts, ","), qIdent(keyCol)))
			sb.WriteString(fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s) ORDER BY %s;   -- 归档表\n\n", qIdent(archiveTable), qIdent(keyCol), strings.Join(parts, ","), qIdent(keyCol)))
		}
	}
	return sb.String()
}

// BuildSampleReviewSQL 生成独立抽样动作的复查 SQL：只含抽样小节，不夹带记录数/求和等项。
// 头部标注种子，抽中的主键附两表回读语句，便于人工对照和复现。
func BuildSampleReviewSQL(sourceTable, archiveTable, condition, keyCol string, results []ReviewItemResult, sampleRounds [][]int64, seed int64, tols Tolerances) string {
	return BuildSampleReviewSQLWithArchiveCondition(sourceTable, archiveTable, condition, "", keyCol, results, sampleRounds, seed, tols)
}

// BuildSampleReviewSQLWithArchiveCondition 在附加归档表过滤条件下生成独立抽样复查 SQL。
// 删除续跑时传入"主键 > 上次删除位置"，把人工复查的范围也限定在尚未删除的部分。
func BuildSampleReviewSQLWithArchiveCondition(sourceTable, archiveTable, condition, archiveCondition, keyCol string, results []ReviewItemResult, sampleRounds [][]int64, seed int64, tols Tolerances) string {
	var sb strings.Builder
	sb.WriteString("-- 独立抽样核对复查 SQL\n")
	sb.WriteString(fmt.Sprintf("-- 生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("-- 源表: %s    归档表: %s\n", sourceTable, archiveTable))
	if condition != "" {
		sb.WriteString(fmt.Sprintf("-- 迁移条件: %s\n", condition))
	}
	if archiveCondition != "" {
		sb.WriteString(fmt.Sprintf("-- 归档表抽样范围: %s（续跑时排除已删部分）\n", archiveCondition))
	}
	sb.WriteString(fmt.Sprintf("-- 抽样种子: %d（加 -seed %d 可复现本轮抽样）\n", seed, seed))
	sb.WriteString("-- 说明: 每项给出可直接执行的 SQL，注释里是工具判定，供人工对照。\n\n")

	sb.WriteString("-- ========== 工具判定汇总 ==========\n")
	for _, r := range results {
		sb.WriteString(fmt.Sprintf("-- [%s] %s：%s\n", r.Verdict, r.Label, r.Detail))
	}
	sb.WriteString("\n")

	sb.WriteString("-- ========== 核对项：随机抽样逐字段比对（抓行级错位） ==========\n")
	sb.WriteString("-- 抽样方式：主键分区随机取点（候选不足时降级为蓄水池抽样）；下面是抽中的主键，可回读两表所有可比对列（生成列除外）人工比对。\n")
	archiveExtra := ""
	if archiveCondition != "" {
		archiveExtra = " AND (" + archiveCondition + ")"
	}
	for r, keys := range sampleRounds {
		if len(keys) == 0 {
			continue
		}
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%d", k))
		}
		sb.WriteString(fmt.Sprintf("-- 第 %d 轮抽中 %d 行\n", r+1, len(keys)))
		sb.WriteString(fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s) ORDER BY %s;   -- 源表\n", qIdent(sourceTable), qIdent(keyCol), strings.Join(parts, ","), qIdent(keyCol)))
		sb.WriteString(fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s)%s ORDER BY %s;   -- 归档表\n\n", qIdent(archiveTable), qIdent(keyCol), strings.Join(parts, ","), archiveExtra, qIdent(keyCol)))
	}
	return sb.String()
}

// buildFieldReview 按字段类型生成对应的复查 SQL。
func buildFieldReview(f ReviewField, sourceTable, archiveTable, srcWhere, archiveWhere string, tols Tolerances) string {
	sumExpr := fmt.Sprintf("SUM(%s)", qIdent(f.Name))
	switch f.Kind {
	case NumericInteger:
		return fmt.Sprintf("-- 字段 %s [整数] —— 零容差精确判定，两侧数值必须逐位相等，差 1 即失败\n"+
			"SELECT %s AS 源表求和 FROM %s%s;\n"+
			"SELECT %s AS 归档表求和 FROM %s%s;\n",
			f.Name, sumExpr, qIdent(sourceTable), srcWhere, sumExpr, qIdent(archiveTable), archiveWhere)
	case NumericDecimal:
		return fmt.Sprintf("-- 字段 %s [定点小数] —— 零容差精确判定，按数值相等，末尾多余的 0 不算差异\n"+
			"SELECT %s AS 源表求和 FROM %s%s;\n"+
			"SELECT %s AS 归档表求和 FROM %s%s;\n",
			f.Name, sumExpr, qIdent(sourceTable), srcWhere, sumExpr, qIdent(archiveTable), archiveWhere)
	case NumericFloat, NumericDouble:
		tol := tols.For(f.Kind)
		return fmt.Sprintf("-- 字段 %s [%s] —— 容差判定：差值绝对值 <= %g + %g × 较大绝对值\n"+
			"SET @src_sum = (SELECT %s FROM %s%s);\n"+
			"SET @dst_sum = (SELECT %s FROM %s%s);\n"+
			"SELECT @src_sum AS 源表求和, @dst_sum AS 归档表求和,\n"+
			"       ABS(@src_sum - @dst_sum) <= %g + %g * GREATEST(ABS(@src_sum), ABS(@dst_sum)) AS 是否在容差内;\n"+
			"-- 返回 1 = 通过，0 = 不通过\n",
			f.Name, f.Kind, tol.Abs, tol.Rel,
			sumExpr, qIdent(sourceTable), srcWhere,
			sumExpr, qIdent(archiveTable), archiveWhere,
			tol.Abs, tol.Rel)
	default:
		return fmt.Sprintf("-- 字段 %s [%s] —— 非数值列，不生成求和比对\n", f.Name, f.Kind)
	}
}

// OutputReviewSQL 把复查 SQL 同时写入文件并打印到控制台，两处内容完全一致。
func OutputReviewSQL(outputDir, sourceTable string, sqlText string) (string, error) {
	return outputSQL(outputDir, fmt.Sprintf("review_%s_%s.sql", sourceTable, time.Now().Format("20060102_150405")), sqlText)
}

// OutputSampleSQL 输出独立抽样动作的复查 SQL，文件名带种子，
// 避免同一秒内反复执行时互相覆盖。
func OutputSampleSQL(outputDir, sourceTable string, seed int64, sqlText string) (string, error) {
	return outputSQL(outputDir, fmt.Sprintf("sample_%s_seed%d_%s.sql", sourceTable, seed, time.Now().Format("20060102_150405")), sqlText)
}

// outputSQL 写文件并原样打印到控制台，两处内容完全一致。
func outputSQL(outputDir, name, sqlText string) (string, error) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", fmt.Errorf("创建输出目录失败: %w", err)
	}
	path := filepath.Join(outputDir, name)
	if err := os.WriteFile(path, []byte(sqlText), 0644); err != nil {
		return "", fmt.Errorf("写复查 SQL 文件失败: %w", err)
	}
	fmt.Println(sqlText)
	fmt.Printf("复查 SQL 已保存到: %s\n", path)
	return path, nil
}
