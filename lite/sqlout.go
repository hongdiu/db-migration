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
	Label  string // 核对项名称
	Verdict string // 通过 / 不通过
	Detail string // 判定说明
}

// BuildReviewSQL 生成完整的复查 SQL 文本。
// sampleRounds 是逐轮抽中的主键清单；传 nil 表示未开启抽样，不输出抽样小节。
func BuildReviewSQL(sourceTable, archiveTable, condition, keyCol string, fields []ReviewField, results []ReviewItemResult, sampleRounds [][]int64, absTol, relTol float64) string {
	return BuildReviewSQLWithArchiveCondition(sourceTable, archiveTable, condition, "", keyCol, fields, results, sampleRounds, absTol, relTol)
}

// BuildReviewSQLWithArchiveCondition 在附加归档表过滤条件下生成复查 SQL。
// 删除续跑时传入"主键 > 上次删除位置"，让人工复查看到的范围与工具校验一致。
func BuildReviewSQLWithArchiveCondition(sourceTable, archiveTable, condition, archiveCondition, keyCol string, fields []ReviewField, results []ReviewItemResult, sampleRounds [][]int64, absTol, relTol float64) string {
	var sb strings.Builder
	sb.WriteString("-- 数据迁移复查 SQL\n")
	sb.WriteString(fmt.Sprintf("-- 生成时间: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("-- 源表: %s    归档表: %s\n", sourceTable, archiveTable))
	if condition != "" {
		sb.WriteString(fmt.Sprintf("-- 迁移条件: %s\n", condition))
	}
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
		sb.WriteString(buildFieldReview(f, sourceTable, archiveTable, srcWhere, archiveWhere, absTol, relTol))
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
		sb.WriteString("-- 抽样方式：主键分区随机取点（候选不足时降级为蓄水池抽样）；下面是抽中的主键，可回读两表全部字段人工比对。\n")
		for r, keys := range sampleRounds {
			if len(keys) == 0 {
				continue
			}
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%d", k))
			}
			sb.WriteString(fmt.Sprintf("-- 第 %d 轮抽中 %d 行\n", r+1, len(keys)))
			// 给出按主键回读两表全部字段的 SQL，用户可直接贴到客户端对照。
			sb.WriteString(fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s) ORDER BY %s;   -- 源表\n", qIdent(sourceTable), qIdent(keyCol), strings.Join(parts, ","), qIdent(keyCol)))
			sb.WriteString(fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s) ORDER BY %s;   -- 归档表\n\n", qIdent(archiveTable), qIdent(keyCol), strings.Join(parts, ","), qIdent(keyCol)))
		}
	}
	return sb.String()
}

// buildFieldReview 按字段类型生成对应的复查 SQL。
func buildFieldReview(f ReviewField, sourceTable, archiveTable, srcWhere, archiveWhere string, absTol, relTol float64) string {
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
	case NumericFloat:
		return fmt.Sprintf("-- 字段 %s [浮点] —— 容差判定：差值绝对值 <= %g + %g × 较大绝对值\n"+
			"SET @src_sum = (SELECT %s FROM %s%s);\n"+
			"SET @dst_sum = (SELECT %s FROM %s%s);\n"+
			"SELECT @src_sum AS 源表求和, @dst_sum AS 归档表求和,\n"+
			"       ABS(@src_sum - @dst_sum) <= %g + %g * GREATEST(ABS(@src_sum), ABS(@dst_sum)) AS 是否在容差内;\n"+
			"-- 返回 1 = 通过，0 = 不通过\n",
			f.Name, absTol, relTol,
			sumExpr, qIdent(sourceTable), srcWhere,
			sumExpr, qIdent(archiveTable), archiveWhere,
			absTol, relTol)
	default:
		return fmt.Sprintf("-- 字段 %s [%s] —— 非数值列，不生成求和比对\n", f.Name, f.Kind)
	}
}

// OutputReviewSQL 把复查 SQL 同时写入文件并打印到控制台，两处内容完全一致。
func OutputReviewSQL(outputDir, sourceTable string, sqlText string) (string, error) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", fmt.Errorf("创建输出目录失败: %w", err)
	}
	stamp := time.Now().Format("20060102_150405")
	path := filepath.Join(outputDir, fmt.Sprintf("review_%s_%s.sql", sourceTable, stamp))
	if err := os.WriteFile(path, []byte(sqlText), 0644); err != nil {
		return "", fmt.Errorf("写复查 SQL 文件失败: %w", err)
	}
	// 控制台原样输出，与文件内容一致。
	fmt.Println(sqlText)
	fmt.Printf("复查 SQL 已保存到: %s\n", path)
	return path, nil
}
