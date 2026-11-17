package dbmigration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Stage 表示条件迁移的阶段。
type Stage string

const (
	StageCopy   Stage = "copy"   // 复制到历史表
	StagePurge  Stage = "purge"  // 从源表删除
	StageVerify Stage = "verify" // 核对
)

// Checkpoint 是断点续跑的进度数据。
type Checkpoint struct {
	Table         string `json:"table"`
	Stage         string `json:"stage"`
	Fingerprint   string `json:"fingerprint"` // 配置指纹，防止改配置后续跑
	HistoryTable  string `json:"history_table"`
	LastKey       int64  `json:"last_key"`       // 已处理到的主键游标
	ProcessedRows int64  `json:"processed_rows"` // 已处理行数
	StartedAt     string `json:"started_at"`
	UpdatedAt     string `json:"updated_at"`
}

// fingerprint 根据表配置生成指纹。任何影响数据归属的字段变化都会导致指纹变化。
func fingerprint(tc *TableConfig) string {
	parts := []string{
		tc.Name, tc.Mode, tc.Condition, tc.HistorySuffix,
		tc.KeyColumn, fmt.Sprintf("%v", tc.UseHistoryTimestamp()),
	}
	raw := strings.Join(parts, "\x1f")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// checkpointPath 返回进度文件路径。
func checkpointPath(dir, table string, stage Stage) string {
	name := fmt.Sprintf(".%s.%s.checkpoint.json", table, stage)
	return filepath.Join(dir, name)
}

// loadCheckpoint 读取进度文件。不存在时返回 nil。
func loadCheckpoint(dir, table string, stage Stage) (*Checkpoint, error) {
	path := checkpointPath(dir, table, stage)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取进度文件失败: %w", err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("解析进度文件失败: %w", err)
	}
	return &cp, nil
}

// saveCheckpoint 采用先写临时文件再原子改名的方式保存进度，避免半写。
func saveCheckpoint(dir string, cp *Checkpoint) error {
	if err := os.MkdirAll(filepath.Clean(dir), 0o755); err != nil {
		return fmt.Errorf("创建进度目录失败: %w", err)
	}
	path := checkpointPath(dir, cp.Table, Stage(cp.Stage))
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化进度失败: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写临时进度文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("提交进度文件失败: %w", err)
	}
	return nil
}

// removeCheckpoint 删除进度文件（阶段完成后调用）。
func removeCheckpoint(dir, table string, stage Stage) error {
	path := checkpointPath(dir, table, stage)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除进度文件失败: %w", err)
	}
	return nil
}

// validFor 校验进度是否适用于当前配置与历史表。
func (cp *Checkpoint) validFor(tc *TableConfig, historyTable string) bool {
	return cp != nil &&
		cp.Fingerprint == fingerprint(tc) &&
		cp.HistoryTable == historyTable &&
		(cp.Stage == string(StageCopy) || cp.Stage == string(StagePurge))
}
