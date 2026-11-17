package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tc := &TableConfig{Name: "orders", Mode: "conditional", Condition: "id > 0", HistorySuffix: "_history", KeyColumn: "id"}
	timestamp := true
	tc.HistoryTimestamp = &timestamp
	cp := &Checkpoint{
		Table: "orders", Stage: string(StageCopy), Fingerprint: fingerprint(tc),
		HistoryTable: "orders_history_20260101", LastKey: 12345, ProcessedRows: 5000,
		StartedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:01:00Z",
	}
	if err := saveCheckpoint(dir, cp); err != nil {
		t.Fatalf("保存进度失败: %v", err)
	}
	got, err := loadCheckpoint(dir, "orders", StageCopy)
	if err != nil {
		t.Fatalf("读取进度失败: %v", err)
	}
	if got == nil {
		t.Fatal("进度不应为空")
	}
	if got.LastKey != 12345 || got.ProcessedRows != 5000 {
		t.Errorf("进度数据错误: %+v", got)
	}
	if !got.validFor(tc, "orders_history_20260101") {
		t.Error("进度应适用于当前配置")
	}
}

func TestCheckpointInvalidOnConfigChange(t *testing.T) {
	tc1 := &TableConfig{Name: "orders", Mode: "conditional", Condition: "id > 0", HistorySuffix: "_history", KeyColumn: "id"}
	tc2 := &TableConfig{Name: "orders", Mode: "conditional", Condition: "id > 100", HistorySuffix: "_history", KeyColumn: "id"}
	if fingerprint(tc1) == fingerprint(tc2) {
		t.Error("不同筛选条件应产生不同指纹")
	}
	cp := &Checkpoint{Table: "orders", Stage: string(StageCopy), Fingerprint: fingerprint(tc1), HistoryTable: "orders_history_x"}
	if cp.validFor(tc2, "orders_history_x") {
		t.Error("配置变更后进度应失效")
	}
	if cp.validFor(tc1, "orders_history_other") {
		t.Error("历史表变更后进度应失效")
	}
}

func TestCheckpointRemove(t *testing.T) {
	dir := t.TempDir()
	cp := &Checkpoint{Table: "t", Stage: string(StagePurge), LastKey: 1}
	if err := saveCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".t.purge.checkpoint.json")); err != nil {
		t.Fatal("进度文件应存在")
	}
	if err := removeCheckpoint(dir, "t", StagePurge); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".t.purge.checkpoint.json")); !os.IsNotExist(err) {
		t.Error("进度文件应已删除")
	}
}

func TestLoadCheckpointMissing(t *testing.T) {
	cp, err := loadCheckpoint(t.TempDir(), "none", StageCopy)
	if err != nil {
		t.Fatal(err)
	}
	if cp != nil {
		t.Error("不存在的进度应返回 nil")
	}
}
