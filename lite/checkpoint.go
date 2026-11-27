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
	Fingerprint  string `json:"fingerprint"` // 配置指纹
	Table        string `json:"table"`
	LastKey      int64  `json:"last_key"`       // 搬运已完成的最后一批的最大主键
	RowsDone     int64  `json:"rows_done"`      // 已搬运行数
	PurgeLastKey int64  `json:"purge_last_key"` // 删除已完成的最后一批的最大主键（续跑起点）
	PurgeDeleted int64  `json:"purge_deleted"`  // 删除已累计行数（含之前运行）
	// 以下是"删除意图"：本批已发起删除、但还没确认落进 PurgeLastKey 的区间 (From, To]。
	// 先记意图再删，崩溃后就能靠源表剩余行数判断这批到底删没删，
	// 不必依赖"删除 + 写进度"在同一个事务里（一批几万行的 DELETE 放进事务代价太高）。
	PurgePendingFrom int64  `json:"purge_pending_from"`
	PurgePendingTo   int64  `json:"purge_pending_to"`
	PurgePendingRows int64  `json:"purge_pending_rows"` // 0 表示没有未确认的批次
	Stage            string `json:"stage"`              // copied | purging | purged
}

// PurgeProgress 从进度文件里提取删除续跑所需的起点。
// 只有处于 purging 阶段的进度才可作为续跑依据；其他阶段一律从头开始。
type PurgeProgress struct {
	StartKey      int64
	DeletedBefore int64
	Resuming      bool
}

// JudgePurgeProgress 判断进度文件能否用于删除续跑。
// 配置指纹不一致时拒绝续跑，避免用旧进度删新条件的数据。
func JudgePurgeProgress(cp *Checkpoint, fingerprint string) (PurgeProgress, error) {
	if cp == nil || cp.Stage != "purging" {
		return PurgeProgress{}, nil
	}
	if cp.Fingerprint != fingerprint {
		// 这里不能建议"删进度文件重来"：已经删掉的数据不会回到源表，
		// 从起点重新校验会永远差那批已删行数，直接卡死。
		return PurgeProgress{}, fmt.Errorf("删除进度文件指纹 %s 与当前配置 %s 不一致。"+
			"已删除的数据不会回到源表，删掉进度文件重来会让两侧永久对不上。"+
			"请把配置改回删除开始时的值；若确认要换配置，需先人工把进度文件里的 purge_last_key/purge_deleted 改成实际删除位置", cp.Fingerprint, fingerprint)
	}
	if cp.PurgeLastKey < 0 || cp.PurgeDeleted < 0 {
		return PurgeProgress{}, fmt.Errorf("删除进度文件内容异常（last=%d deleted=%d），拒绝续跑", cp.PurgeLastKey, cp.PurgeDeleted)
	}
	return PurgeProgress{StartKey: cp.PurgeLastKey, DeletedBefore: cp.PurgeDeleted, Resuming: true}, nil
}

// PurgeRecovery 是对"上次已发起、尚未确认"的那批删除的判定结果。
type PurgeRecovery struct {
	HasPending bool  // 进度文件里有未确认的批次
	Applied    bool  // 该批已确认删除完成
	StartKey   int64 // 恢复后应使用的删除起点（已删时为该批终点）
	Deleted    int64 // 恢复后应使用的累计删除行数
}

// JudgePendingPurge 根据"本批区间内源表剩余行数"判定上次发起的那批删除是否已生效。
//
// 背景：删除和写进度不在同一个事务里。一批 DELETE 提交后、进度落盘前进程被杀，
// 磁盘上的进度仍停在这批之前。这时不能盲目重删（会白删一轮），也不能删掉进度文件重来
// （源表已少一批、归档表没少，两侧永远差那批行数，校验会永久拒绝删除）。
//
// 单条 DELETE 在 InnoDB 内部是原子的，所以正常情况下只会有两种结果：
//   - remaining == 0       → 这批已经删掉了，把进度推进到 To，并补记删除行数；
//   - remaining == planned → 完全没删，进度保持原样，重试这一批即可。
//
// 其他数值说明期间有外部写入或删除，无法凭剩余行数猜出真实状态，停下来交人工处理。
func JudgePendingPurge(cp *Checkpoint, remaining int64) (PurgeRecovery, error) {
	if cp == nil || cp.PurgePendingRows <= 0 {
		return PurgeRecovery{}, nil
	}
	if cp.PurgePendingTo <= cp.PurgePendingFrom {
		// 区间是左开右闭的 (From, To]，两端相等时区间为空，不可能对应任何计划删除行。
		return PurgeRecovery{}, fmt.Errorf("进度文件中的删除意图区间异常：[%d, %d]，请人工核对进度文件",
			cp.PurgePendingFrom, cp.PurgePendingTo)
	}
	switch remaining {
	case 0:
		return PurgeRecovery{
			HasPending: true,
			Applied:    true,
			StartKey:   cp.PurgePendingTo,
			Deleted:    cp.PurgeDeleted + cp.PurgePendingRows,
		}, nil
	case cp.PurgePendingRows:
		return PurgeRecovery{HasPending: true, Applied: false}, nil
	default:
		return PurgeRecovery{}, fmt.Errorf("上次删除的批次状态无法自动判定：区间 (%d, %d] 计划删 %d 行，源表还剩 %d 行。"+
			"可能有外部写入或删除夹在中间。请人工核对该区间后，把进度文件里的 purge_last_key/purge_deleted 改成实际值，再重跑",
			cp.PurgePendingFrom, cp.PurgePendingTo, cp.PurgePendingRows, remaining)
	}
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
