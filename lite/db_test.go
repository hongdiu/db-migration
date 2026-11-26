package main

import (
	"strings"
	"testing"
)

func TestJudgePrimaryKeyUsable(t *testing.T) {
	// 整型主键通过。
	key, err := judgePrimaryKeyUsable("", &PrimaryKeyInfo{Column: "id", DataType: "bigint"})
	if err != nil || key != "id" {
		t.Fatalf("整型主键应通过: key=%s err=%v", key, err)
	}
	// 配置与探测一致通过。
	if _, err := judgePrimaryKeyUsable("ID", &PrimaryKeyInfo{Column: "id", DataType: "int"}); err != nil {
		t.Fatalf("大小写不一致应视为相同: %v", err)
	}
	// 配置与探测不一致拒绝。
	if _, err := judgePrimaryKeyUsable("uid", &PrimaryKeyInfo{Column: "id", DataType: "int"}); err == nil {
		t.Fatal("配置主键与实际主键不一致应拒绝")
	}
	// 非整型主键拒绝。
	if _, err := judgePrimaryKeyUsable("", &PrimaryKeyInfo{Column: "uid", DataType: "varchar"}); err == nil {
		t.Fatal("字符串主键应拒绝")
	}
	// 复合主键（nil 模拟未通过探测）拒绝。
	if _, err := judgePrimaryKeyUsable("", nil); err == nil {
		t.Fatal("无主键应拒绝")
	}
}

func TestJudgeDependenciesAllowed(t *testing.T) {
	if err := judgeDependenciesAllowed(nil); err != nil {
		t.Fatalf("无依赖应允许: %v", err)
	}
	deps := []DependencyInfo{
		{Kind: "foreign_key", Name: "order_items", Ref: "fk_order"},
		{Kind: "view", Name: "v_orders", Ref: "..."},
	}
	err := judgeDependenciesAllowed(deps)
	if err == nil {
		t.Fatal("有依赖应拒绝")
	}
	msg := err.Error()
	if !strings.Contains(msg, "foreign_key") || !strings.Contains(msg, "view") {
		t.Fatalf("错误信息应列出两类依赖: %s", msg)
	}
}

func TestJudgeArchiveTableFree(t *testing.T) {
	if err := judgeArchiveTableFree(false, "orders_archive"); err != nil {
		t.Fatalf("归档表不存在应允许: %v", err)
	}
	if err := judgeArchiveTableFree(true, "orders_archive"); err == nil {
		t.Fatal("归档表已存在应拒绝")
	}
}

func TestIsGeneratedColumn(t *testing.T) {
	if !isGeneratedColumn("VIRTUAL GENERATED") {
		t.Fatal("虚拟生成列应识别")
	}
	if !isGeneratedColumn("STORED GENERATED") {
		t.Fatal("存储生成列应识别")
	}
	if isGeneratedColumn("") || isGeneratedColumn("auto_increment") {
		t.Fatal("普通列不应误判为生成列")
	}
}

func TestQIdent(t *testing.T) {
	if qIdent("orders") != "`orders`" {
		t.Fatalf("反引号包裹失败: %s", qIdent("orders"))
	}
}
