package dbmigration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本文件解决“全表迁移后无法复核”的问题：
// 全表迁移用原子改名把源表变成历史表，源表随即变成空表。如果核对只在迁移后做，
// 或事后用 -verify-only 再复核，源表已经是 0 行，基线无从重建。
// 因此迁移前把基线（含抽样行快照）落盘，事后复核直接读这份基线。

// sumSnapshot 是可 JSON 化的精确求和值。
type sumSnapshot struct {
	Raw  string      `json:"raw"`
	Kind NumericKind `json:"kind"`
}

// minMaxSnapshot 是可 JSON 化的最值。
type minMaxSnapshot struct {
	Kind    NumericKind `json:"kind"`
	MinText string      `json:"min_text"`
	MaxText string      `json:"max_text"`
}

// baselineFile 是基线文件的磁盘格式。
type baselineFile struct {
	Table       string                    `json:"table"`
	Mode        string                    `json:"mode"`
	Condition   string                    `json:"condition"`
	Fingerprint string                    `json:"fingerprint"`
	SchemaHash  string                    `json:"schema_hash"` // 源表列结构指纹
	CapturedAt  string                    `json:"captured_at"`
	Count       int64                     `json:"count"`
	Sums        map[string]sumSnapshot    `json:"sums"`
	Nonempty    map[string]int64          `json:"nonempty"`
	MinMax      map[string]minMaxSnapshot `json:"min_max"`
	Snaps       [][]*RowSnapshot          `json:"snaps"`
	Kinds       map[string]NumericKind    `json:"kinds"`
	SampleSeed  int64                     `json:"sample_seed"`
}

// computeSchemaHash 计算列结构指纹：按字段顺序拼接 列名|数据类型|额外属性，取 SHA256。
// 用于事后复核时比对“基线采集时的表结构”与“当前目标表结构”是否一致。
func computeSchemaHash(cols []ColumnMeta) string {
	var sb strings.Builder
	for _, c := range cols {
		sb.WriteString(strings.ToLower(c.Name))
		sb.WriteByte('|')
		sb.WriteString(strings.ToLower(c.DataType))
		sb.WriteByte('|')
		sb.WriteString(strings.ToLower(c.Extra))
		sb.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])[:16]
}

// baselinePath 返回基线文件路径。每张表每种模式一份，迁移前覆盖写入最近一次的基线。
func baselinePath(dir string, tc *TableConfig) string {
	return filepath.Join(dir, fmt.Sprintf(".baseline.%s.%s.json", tc.Name, tc.Mode))
}

// saveBaseline 原子写入基线文件（先写临时文件再改名，避免半写损坏）。
func saveBaseline(dir string, tc *TableConfig, b *baseline) (string, error) {
	if err := os.MkdirAll(filepath.Clean(dir), 0o755); err != nil {
		return "", fmt.Errorf("创建基线目录失败: %w", err)
	}
	f := &baselineFile{
		Table:       tc.Name,
		Mode:        tc.Mode,
		Condition:   tc.Condition,
		Fingerprint: fingerprint(tc),
		SchemaHash:  b.schemaHash,
		CapturedAt:  baselineCapturedAt(b).Format(time.RFC3339),
		Count:       b.count,
		Sums:        make(map[string]sumSnapshot, len(b.sums)),
		Nonempty:    b.nonempty,
		MinMax:      make(map[string]minMaxSnapshot, len(b.minmax)),
		Snaps:       b.snaps,
		Kinds:       b.kinds,
		SampleSeed:  b.sampleSeed,
	}
	for name, n := range b.sums {
		if n == nil {
			continue
		}
		f.Sums[name] = sumSnapshot{Raw: n.Raw, Kind: n.Kind}
	}
	for name, v := range b.minmax {
		if v == nil {
			continue
		}
		f.MinMax[name] = minMaxSnapshot{Kind: v.Kind, MinText: v.MinText, MaxText: v.MaxText}
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", fmt.Errorf("序列化基线失败: %w", err)
	}
	path := baselinePath(dir, tc)
	tmp := path + ".tmp"
	// 基线里含抽样行的原始字段值，属于业务数据副本，权限收紧到仅属主可读写。
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", fmt.Errorf("写基线临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("提交基线文件失败: %w", err)
	}
	return path, nil
}

// baselineCapturedAt 返回基线的采集时刻，缺失时退化为当前时间。
func baselineCapturedAt(b *baseline) time.Time {
	if b != nil && !b.capturedAt.IsZero() {
		return b.capturedAt
	}
	return time.Now()
}

// loadBaseline 读取基线文件并还原为内存结构。数值按记录的类别重新精确解析。
func loadBaseline(path string) (*baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f baselineFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("解析基线文件 %s 失败: %w", path, err)
	}

	b := &baseline{
		count:    f.Count,
		sums:     make(map[string]*ExactNumber, len(f.Sums)),
		nonempty: f.Nonempty,
		minmax:   make(map[string]*MinMaxValue, len(f.MinMax)),
		snaps:    f.Snaps,
		kinds:    f.Kinds,

		schemaHash: f.SchemaHash,
		sourcePath: path,
		sampleSeed: f.SampleSeed,
	}
	if ts, perr := time.Parse(time.RFC3339, f.CapturedAt); perr == nil {
		b.capturedAt = ts
	}
	if b.nonempty == nil {
		b.nonempty = map[string]int64{}
	}
	if b.kinds == nil {
		b.kinds = map[string]NumericKind{}
	}
	for name, s := range f.Sums {
		n, err := ParseExactNumber(s.Raw, s.Kind)
		if err != nil {
			return nil, fmt.Errorf("基线中字段 %s 的求和值 %q 无法解析: %w", name, s.Raw, err)
		}
		b.sums[name] = n
	}
	for name, m := range f.MinMax {
		v := &MinMaxValue{Kind: m.Kind, MinText: m.MinText, MaxText: m.MaxText}
		if m.Kind != NumericNone && m.MinText != "" {
			if n, err := ParseExactNumber(m.MinText, m.Kind); err == nil {
				v.MinNum = n
			}
		}
		if m.Kind != NumericNone && m.MaxText != "" {
			if n, err := ParseExactNumber(m.MaxText, m.Kind); err == nil {
				v.MaxNum = n
			}
		}
		b.minmax[name] = v
	}
	return b, nil
}

// checkBaselineCompatible 校验文件基线是否可用于本次复核。
// 这里刻意做成硬错误而不是警告：基线一旦与当前表结构/配置不匹配，
// 后续所有比对结论都失去意义，继续跑只会给出“看起来通过”的假象。
func checkBaselineCompatible(b *baseline, tc *TableConfig, cols []ColumnMeta) error {
	if b.schemaHash == "" {
		return fmt.Errorf("基线文件 %s 不含表结构指纹（由旧版本生成），无法确认表结构是否变更。"+
			"请重新执行一次迁移/复制以生成新基线", b.sourcePath)
	}
	want := computeSchemaHash(cols)
	if b.schemaHash != want {
		return fmt.Errorf("目标表 %s 的结构与基线不一致（基线指纹 %s，当前指纹 %s）："+
			"期间可能执行过 DDL，用旧基线核对已无意义。请确认结构变更范围后重新执行一次迁移/复制以重建基线",
			tc.Name, b.schemaHash, want)
	}
	return nil
}

// loadBaselineForVerify 读取文件基线并校验其与目标历史表是否匹配。
// 返回的 notice 用于写入报告，明确标注“这是文件基线，不是实时采集”。
func loadBaselineForVerify(ctx context.Context, db *sql.DB, dir string, tc *TableConfig, targetTable string) (*baseline, string, error) {
	path := baselinePath(dir, tc)
	b, err := loadBaseline(path)
	if err != nil {
		return nil, "", err
	}
	cols, err := tableColumns(ctx, db, targetTable)
	if err != nil {
		return nil, "", fmt.Errorf("读取目标表 %s 的结构失败: %w", targetTable, err)
	}
	if err := checkBaselineCompatible(b, tc, cols); err != nil {
		return nil, "", err
	}
	captured := "未知（文件未记录）"
	if !b.capturedAt.IsZero() {
		captured = b.capturedAt.Format(time.RFC3339)
	}
	notice := fmt.Sprintf("%s（采集于 %s，结构指纹 %s）", path, captured, b.schemaHash)
	return b, notice, nil
}
