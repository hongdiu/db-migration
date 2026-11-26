// 断点续跑：按主键游标记录每张表的搬运进度。
//
// 关键设计：
//   - 进度文件内容包含配置指纹，配置变了就拒绝续跑，避免拿旧进度套新条件导致错数据。
//   - 原子写：先写临时文件再改名，进程被 kill 也不会留下半截进度。
//   - 每批提交后立即落进度，中断重启从下一批继续，不重不漏。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Checkpoint 是一张表的搬运进度。
type Checkpoint struct {
	Fingerprint string `json:"fingerprint"` // 配置指纹
	Table       string `json:"table"`
	LastKey     int64  `json:"last_key"`   // 已完成的最后一批的最大主键
	RowsDone    int64  `json:"rows_done"`  // 已搬运行数
	Stage       string `json:"stage"`      // copying | copied | verified | purged
}

// ConfigFingerprint 由表名、模式、条件、批大小生成指纹。
// 这些任一变化都会让"续跑旧进度"变得不安全，必须拒绝。
func ConfigFingerprint(t TableCfg) string {
	raw := fmt.Sprintf("%s|%s|%s|%s|%d|%v|%d|%d",
		t.Name, t.Mode, t.ArchiveTable, t.Condition, t.BatchSize, t.Verify.NumericFields,
		t.Verify.Rounds(), t.Verify.PerRound())
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

func checkpointPath(dir, table string) string {
	return filepath.Join(dir, fmt.Sprintf("cp_%s.json", table))
}

// LoadCheckpoint 读取进度；文件不存在返回 nil, nil。
func LoadCheckpoint(dir, table string) (*Checkpoint, error) {
	path := checkpointPath(dir, table)
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

// JudgeCheckpointResumable 判断旧进度能否用于本次配置。
func JudgeCheckpointResumable(cp *Checkpoint, fingerprint string) error {
	if cp == nil {
		return nil
	}
	if cp.Fingerprint != fingerprint {
		return fmt.Errorf("进度文件指纹 %s 与当前配置 %s 不一致，条件或表配置可能已更改；请确认后删除进度文件重新开始", cp.Fingerprint, fingerprint)
	}
	return nil
}

// SaveCheckpoint 原子写入进度：写临时文件 → 改名覆盖。
func SaveCheckpoint(dir, table string, cp *Checkpoint) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建进度目录失败: %w", err)
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化进度失败: %w", err)
	}
	path := checkpointPath(dir, table)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("写临时进度文件失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("提交进度文件失败: %w", err)
	}
	return nil
}

// RemoveCheckpoint 删除进度文件（全部完成后清理）。
func RemoveCheckpoint(dir, table string) error {
	err := os.Remove(checkpointPath(dir, table))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
