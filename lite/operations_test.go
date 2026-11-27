package main

import (
	"strings"
	"testing"
)

func TestBuildCopyQuery(t *testing.T) {
	q := BuildCopyQuery("orders", "orders_archive", []string{"`id`", "`amount`"}, "id", "created_at < '2024-01-01'", 100, 5000)
	if !strings.Contains(q, "`id` > 100") {
		t.Fatalf("应带主键游标条件: %s", q)
	}
	if !strings.Contains(q, "(created_at < '2024-01-01') AND") {
		t.Fatalf("应带迁移条件: %s", q)
	}
	if !strings.Contains(q, "ON DUPLICATE KEY UPDATE") {
		t.Fatalf("应幂等写入: %s", q)
	}
	if !strings.Contains(q, "LIMIT 5000") {
		t.Fatalf("应限制批大小: %s", q)
	}
	// 无条件场景。
	q2 := BuildCopyQuery("logs", "logs_archive", []string{"`id`"}, "id", "", 0, 100)
	if strings.Contains(q2, "() AND") {
		t.Fatalf("无条件时不应有空的括号: %s", q2)
	}
}

func TestJudgePurgeAllowed(t *testing.T) {
	cases := []struct {
		name    string
		v       PurgeVerdict
		allowed bool
	}{
		{"对齐允许", PurgeVerdict{SrcRowsToDelete: 100, DeletedSoFar: 0, ArchiveRows: 100, BatchRows: 50}, true},
		{"续跑对齐允许", PurgeVerdict{SrcRowsToDelete: 50, DeletedSoFar: 50, ArchiveRows: 100, BatchRows: 50}, true},
		{"归档不足拒绝", PurgeVerdict{SrcRowsToDelete: 100, DeletedSoFar: 0, ArchiveRows: 80, BatchRows: 50}, false},
		{"归档过多拒绝", PurgeVerdict{SrcRowsToDelete: 90, DeletedSoFar: 0, ArchiveRows: 100, BatchRows: 50}, false},
		{"本批超待删拒绝", PurgeVerdict{SrcRowsToDelete: 30, DeletedSoFar: 70, ArchiveRows: 100, BatchRows: 50}, false},
		{"零批拒绝", PurgeVerdict{SrcRowsToDelete: 100, DeletedSoFar: 0, ArchiveRows: 100, BatchRows: 0}, false},
		{"负数拒绝", PurgeVerdict{SrcRowsToDelete: -1, DeletedSoFar: 0, ArchiveRows: 100, BatchRows: 10}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := JudgePurgeAllowed(tc.v)
			if tc.allowed && err != nil {
				t.Fatalf("应允许，实际: %v", err)
			}
			if !tc.allowed && err == nil {
				t.Fatal("应拒绝，实际允许")
			}
		})
	}
}

func TestBuildDeleteByKeys(t *testing.T) {
	q := BuildDeleteByKeys("orders", "id", []int64{1, 2, 3})
	if q != "DELETE FROM `orders` WHERE `id` IN (1,2,3)" {
		t.Fatalf("删除 SQL 不对: %s", q)
	}
	if strings.Contains(q, "WHERE created_at") {
		t.Fatal("不能按原条件删除")
	}
}

func TestBuildRemainingCountQuery(t *testing.T) {
	q := BuildRemainingCountQuery("orders", "id", "created_at < '2024-01-01'", 9000000)
	if !strings.Contains(q, "created_at < '2024-01-01'") || !strings.Contains(q, "`id` <= 9000000") {
		t.Fatalf("残留检查应同时带条件和主键上界: %s", q)
	}
}

func TestFullTableSQLBuilders(t *testing.T) {
	if got := BuildCreateEmptyTable("orders", "orders__new_empty"); got != "CREATE TABLE `orders__new_empty` LIKE `orders`" {
		t.Fatalf("建空表 SQL 不对: %s", got)
	}
	if got := BuildAlignAutoIncrement("orders__new_empty", "id", 999); got != "ALTER TABLE `orders__new_empty` AUTO_INCREMENT = 1000" {
		t.Fatalf("自增值对齐 SQL 不对: %s", got)
	}
	rename := BuildRenameTable("orders", "orders_archive", "orders__new_empty")
	if !strings.Contains(rename, "`orders` TO `orders_archive`") || !strings.Contains(rename, "`orders__new_empty` TO `orders`") {
		t.Fatalf("改名 SQL 不对: %s", rename)
	}
	// 改名必须是一条原子语句，不能用分号拆成两条。
	if strings.Contains(rename, ";") {
		t.Fatalf("改名必须是单条原子语句: %s", rename)
	}
}

func TestJudgeMaxKeyForAutoIncrement(t *testing.T) {
	if err := JudgeMaxKeyForAutoIncrement(100); err != nil {
		t.Fatalf("正常值应通过: %v", err)
	}
	if err := JudgeMaxKeyForAutoIncrement(-1); err == nil {
		t.Fatal("负数应拒绝")
	}
}

func TestBuildCopySelectQueryCarriesCondition(t *testing.T) {
	q := BuildCopySelectQuery("trans_label", []string{"`id`"}, "id", "create_time < '2026-09-01 00:00:00'", 5000, 5000)
	if !strings.Contains(q, "(create_time < '2026-09-01 00:00:00') AND `id` > 5000") {
		t.Fatalf("本批 SELECT 必须同时带迁移条件和主键游标: %s", q)
	}
	if !strings.Contains(q, "ORDER BY `id` LIMIT 5000") {
		t.Fatalf("本批 SELECT 必须按主键排序并限制批大小: %s", q)
	}
	q2 := BuildCopySelectQuery("logs", []string{"`id`"}, "id", "", 0, 100)
	if strings.Contains(q2, "() AND") {
		t.Fatalf("无条件时不应出现空括号: %s", q2)
	}
}

func TestBuildCopyQuerySharesWhereWithSelect(t *testing.T) {
	columns := []string{"`id`", "`create_time`"}
	condition := "create_time < '2026-09-01 00:00:00'"
	insertSQL := BuildCopyQuery("src", "dst", columns, "id", condition, 5000, 5000)
	selectSQL := BuildCopySelectQuery("src", columns, "id", condition, 5000, 5000)
	if !strings.Contains(insertSQL, selectSQL) {
		t.Fatalf("搬运 INSERT 必须直接复用本批 SELECT，避免两处条件漂移:\nINSERT=%s\nSELECT=%s", insertSQL, selectSQL)
	}
}

func TestJudgeCopyBatch(t *testing.T) {
	t.Run("空批表示搬完", func(t *testing.T) {
		s, err := JudgeCopyBatch(nil, 100, 5000)
		if err != nil || !s.Done || s.LastKey != 100 || s.RowsDone != 5000 {
			t.Fatalf("空批应保留进度并标记完成: %+v, %v", s, err)
		}
	})

	t.Run("稀疏主键按最后一条推进", func(t *testing.T) {
		// 命中条件的行在千万级表里稀疏分布，本批主键跨度很大。
		keys := []int64{1032672, 2048001, 3072001}
		s, err := JudgeCopyBatch(keys, 500000, 1000)
		if err != nil || s.Done {
			t.Fatalf("应正常推进: %+v, %v", s, err)
		}
		if s.LastKey != 3072001 {
			t.Fatalf("游标应推进到本批最大主键 3072001，实际 %d", s.LastKey)
		}
		if s.RowsDone != 1003 {
			t.Fatalf("行数应按本批实际条数累加为 1003，实际 %d", s.RowsDone)
		}
	})

	t.Run("游标未前进报错", func(t *testing.T) {
		if _, err := JudgeCopyBatch([]int64{100, 90}, 100, 0); err == nil {
			t.Fatal("游标未前进应报错，避免死循环")
		}
	})
}

func TestJudgeCopyCounts(t *testing.T) {
	if err := JudgeCopyCounts(13057038, 13057038); err != nil {
		t.Fatalf("行数一致应通过: %v", err)
	}
	if err := JudgeCopyCounts(13057038, 896188); err == nil {
		t.Fatal("行数不一致应报错")
	} else if !strings.Contains(err.Error(), "12160850") {
		t.Fatalf("错误信息应包含差值 12160850: %v", err)
	}
	if err := JudgeCopyCounts(100, 200); err == nil {
		t.Fatal("归档多于源表也应报错")
	}
}
