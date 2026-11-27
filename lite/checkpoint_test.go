package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJudgePendingPurge(t *testing.T) {
	// 没有未确认批次：不进入恢复流程。
	if rec, err := JudgePendingPurge(&Checkpoint{Stage: "purging", PurgeLastKey: 100}, 0); err != nil || rec.HasPending {
		t.Fatalf("无意图时不应触发恢复: %+v err=%v", rec, err)
	}

	cp := &Checkpoint{
		Stage: "purging", PurgeLastKey: 100, PurgeDeleted: 50,
		PurgePendingFrom: 100, PurgePendingTo: 200, PurgePendingRows: 30,
	}
	// 区间内已无剩余行：这批确实删掉了，进度推到 200，累计行数补上 30。
	rec, err := JudgePendingPurge(cp, 0)
	if err != nil {
		t.Fatalf("已删场景不应报错: %v", err)
	}
	if !rec.HasPending || !rec.Applied || rec.StartKey != 200 || rec.Deleted != 80 {
		t.Fatalf("已删场景恢复结果不对: %+v", rec)
	}
	// 剩余行数等于计划行数：完全没删，进度保持原样，重试这一批。
	rec, err = JudgePendingPurge(cp, 30)
	if err != nil {
		t.Fatalf("未删场景不应报错: %v", err)
	}
	if !rec.HasPending || rec.Applied || rec.StartKey != 0 || rec.Deleted != 0 {
		t.Fatalf("未删场景应保持原进度: %+v", rec)
	}
	// 介于两者之间：可能有外部写入，不能猜。
	if _, err := JudgePendingPurge(cp, 12); err == nil {
		t.Fatal("部分剩余应拒绝自动判定")
	} else if !strings.Contains(err.Error(), "无法自动判定") {
		t.Fatalf("错误信息应说明无法自动判定: %v", err)
	}
	// 区间本身不合法：直接报错。
	bad := &Checkpoint{PurgePendingFrom: 200, PurgePendingTo: 100, PurgePendingRows: 5}
	if _, err := JudgePendingPurge(bad, 0); err == nil {
		t.Fatal("区间倒置应报错")
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cp := &Checkpoint{
		Fingerprint: "abc123",
		Table:       "orders",
		LastKey:     9000000,
		RowsDone:    12345,
		Stage:       "copied",
	}
	if err := SaveCheckpoint(dir, "orders", cp); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := LoadCheckpoint(dir, "orders")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got == nil || *got != *cp {
		t.Fatalf("往返不一致: 期望 %+v，实际 %+v", cp, got)
	}
	// 临时文件应已清理。
	tmp := filepath.Join(dir, "cp_orders.json.tmp")
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("临时文件应被改名清理")
	}
}

func TestLoadCheckpointMissing(t *testing.T) {
	got, err := LoadCheckpoint(t.TempDir(), "missing")
	if err != nil {
		t.Fatalf("不存在不应报错: %v", err)
	}
	if got != nil {
		t.Fatalf("不存在应返回 nil: %+v", got)
	}
}

func TestJudgeCheckpointResumable(t *testing.T) {
	cp := &Checkpoint{Fingerprint: "aaa"}
	if err := JudgeCheckpointResumable(cp, "aaa"); err != nil {
		t.Fatalf("指纹一致应允许续跑: %v", err)
	}
	if err := JudgeCheckpointResumable(cp, "bbb"); err == nil {
		t.Fatal("指纹不一致应拒绝续跑")
	}
	if err := JudgeCheckpointResumable(nil, "bbb"); err != nil {
		t.Fatalf("无进度文件应允许全新开始: %v", err)
	}
}

func TestConfigFingerprintSensitivity(t *testing.T) {
	base := TableCfg{Name: "orders", Mode: ModeConditional, ArchiveTable: "orders_archive", Condition: "id>1", BatchSize: 100}
	f1 := ConfigFingerprint(base)
	if f1 == "" || len(f1) != 16 {
		t.Fatalf("指纹格式不对: %s", f1)
	}
	// 条件变化必须改变指纹。
	changed := base
	changed.Condition = "id>2"
	if ConfigFingerprint(changed) == f1 {
		t.Fatal("条件变化应改变指纹")
	}
	// 校验字段变化必须改变指纹。
	changed2 := base
	changed2.Verify.NumericFields = []string{"amount"}
	if ConfigFingerprint(changed2) == f1 {
		t.Fatal("校验字段变化应改变指纹")
	}
}

func TestRemoveCheckpoint(t *testing.T) {
	dir := t.TempDir()
	cp := &Checkpoint{Fingerprint: "abc", Table: "orders"}
	if err := SaveCheckpoint(dir, "orders", cp); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCheckpoint(dir, "orders"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	got, _ := LoadCheckpoint(dir, "orders")
	if got != nil {
		t.Fatal("删除后应为 nil")
	}
	// 重复删除不报错。
	if err := RemoveCheckpoint(dir, "orders"); err != nil {
		t.Fatalf("重复删除不应报错: %v", err)
	}
}
