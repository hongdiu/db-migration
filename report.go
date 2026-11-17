package dbmigration

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type CheckResult struct {
	Name   string
	Pass   bool
	Detail string
	Rule   string // 使用的核对规则说明
}

type TableReport struct {
	Table      string
	Mode       string
	DryRun     bool
	Stage      string
	HistoryTbl string
	Checks     []CheckResult
	Diffs      []FieldDiff
	Err        error
	OutputDir  string
}

func (r *TableReport) add(name string, pass bool, detail string) {
	r.addWithRule(name, pass, detail, "")
}

func (r *TableReport) addWithRule(name string, pass bool, detail, rule string) {
	r.Checks = append(r.Checks, CheckResult{Name: name, Pass: pass, Detail: detail, Rule: rule})
}

func (r *TableReport) Pass() bool {
	if r.Err != nil {
		return false
	}
	if len(r.Checks) == 0 {
		return false
	}
	for _, c := range r.Checks {
		if !c.Pass {
			return false
		}
	}
	// 抽样差异为空才算通过。
	return len(r.Diffs) == 0
}

func (r *TableReport) Print() {
	fmt.Printf("\n[%s] 模式: %s", r.Table, r.Mode)
	if r.Stage != "" {
		fmt.Printf(" 阶段: %s", r.Stage)
	}
	if r.DryRun {
		fmt.Printf("  (dry-run)")
	}
	fmt.Println()

	if r.Err != nil {
		fmt.Printf("  错误: %v\n", r.Err)
		fmt.Println("  结论: FAIL")
		return
	}

	for _, c := range r.Checks {
		mark := "✓"
		if !c.Pass {
			mark = "✗"
		}
		fmt.Printf("  %s %s: %s", mark, c.Name, c.Detail)
		if c.Rule != "" {
			fmt.Printf(" [规则: %s]", c.Rule)
		}
		fmt.Println()
	}

	if len(r.Diffs) > 0 {
		show := len(r.Diffs)
		if show > 10 {
			show = 10
		}
		fmt.Printf("  字段差异 (展示前 %d / 共 %d 条):\n", show, len(r.Diffs))
		for i := 0; i < show; i++ {
			d := r.Diffs[i]
			fmt.Printf("    轮次=%d key=%d 字段=%s 迁移前=%q 迁移后=%q 原因=%s\n",
				d.Round, d.Key, d.Column, d.Expected, d.Actual, d.Reason)
		}
	}
	if r.Pass() {
		fmt.Println("  结论: PASS")
	} else {
		fmt.Println("  结论: FAIL")
	}
}

// fileStamp 返回带纳秒的时间戳，避免同秒多次运行文件名冲突。
func fileStamp() string {
	return time.Now().Format("20060102_150405.000000000")
}

// WriteDetailFiles 在核对失败时产出结果文件，返回产出路径。
// 只要有任何核对项不通过就产出，不再仅限于“字段差异”：
// 记录数、求和、非空数、最值这类汇总核对失败时同样需要留档，否则现场只剩一行控制台输出。
func (r *TableReport) WriteDetailFiles() ([]string, error) {
	if r.Pass() {
		return nil, nil
	}
	if r.OutputDir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Clean(r.OutputDir), 0o755); err != nil {
		return nil, fmt.Errorf("创建结果目录失败: %w", err)
	}
	base := fmt.Sprintf("%s_%s_%s", r.Table, r.StageOrAction(), fileStamp())

	var files []string

	// 1) 核对项明细：无论失败的是汇总项还是抽样项，都完整落一份。
	checkPath := filepath.Join(r.OutputDir, base+"_checks.csv")
	if err := writeChecksCSV(checkPath, r); err != nil {
		return files, err
	}
	files = append(files, checkPath)

	// 2) 字段差异：仅在有逐字段差异时产出，避免产生空文件。
	if len(r.Diffs) > 0 {
		diffFiles, err := writeDiffFiles(r.OutputDir, base, r.Diffs)
		if err != nil {
			return files, err
		}
		files = append(files, diffFiles...)
	}
	return files, nil
}

// StageOrAction 返回用于文件命名的阶段标识。
func (r *TableReport) StageOrAction() string {
	if r.Stage != "" {
		return r.Stage
	}
	if r.Mode != "" {
		return r.Mode
	}
	return "run"
}

func writeChecksCSV(path string, r *TableReport) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建核对结果CSV失败: %w", err)
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"表", "阶段", "核对项", "结论", "明细", "核对规则"})
	for _, c := range r.Checks {
		verdict := "通过"
		if !c.Pass {
			verdict = "失败"
		}
		_ = w.Write([]string{r.Table, r.StageOrAction(), c.Name, verdict, c.Detail, c.Rule})
	}
	if r.Err != nil {
		_ = w.Write([]string{r.Table, r.StageOrAction(), "<执行错误>", "失败", r.Err.Error(), ""})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return fmt.Errorf("写核对结果CSV失败: %w", err)
	}
	return f.Close()
}

func writeDiffFiles(outputDir, base string, diffs []FieldDiff) ([]string, error) {
	csvPath := filepath.Join(outputDir, base+"_diff.csv")
	jsonPath := filepath.Join(outputDir, base+"_diff.json")

	f, err := os.Create(csvPath)
	if err != nil {
		return nil, fmt.Errorf("创建差异CSV失败: %w", err)
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"表", "轮次", "主键", "字段", "迁移前", "迁移后", "数值类别", "差异原因"})
	for _, d := range diffs {
		_ = w.Write([]string{d.Table, fmt.Sprintf("%d", d.Round), fmt.Sprintf("%d", d.Key),
			d.Column, d.Expected, d.Actual, d.Kind.String(), d.Reason})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return nil, fmt.Errorf("写差异CSV失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	data, err := json.MarshalIndent(diffs, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化差异JSON失败: %w", err)
	}
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("写差异JSON失败: %w", err)
	}
	return []string{csvPath, jsonPath}, nil
}

// WritePurgeList 产出删除清单 CSV。
func WritePurgeList(outputDir, sourceTable string, candidates []PurgeCandidate) (string, error) {
	if err := os.MkdirAll(filepath.Clean(outputDir), 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(outputDir, fmt.Sprintf("%s_%s_purge_list.csv", sourceTable, fileStamp()))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"历史表", "记录数", "业务日期", "保留天数", "到期日"})
	for _, c := range candidates {
		_ = w.Write([]string{c.HistoryTable, fmt.Sprintf("%d", c.RowCount), c.BusinessDate,
			fmt.Sprintf("%d", c.RetentionDays), c.Expiry})
	}
	w.Flush()
	err = w.Error()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return path, err
}

// WriteMergeResult 产出合并清单 JSON。
func WriteMergeResult(outputDir string, res *MergeResult) (string, error) {
	if err := os.MkdirAll(filepath.Clean(outputDir), 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(outputDir, fmt.Sprintf("%s_%s_merge.json", res.SourceTable, fileStamp()))
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
