package main

import (
	"strings"
	"testing"
	"time"
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
	if got := BuildAlignAutoIncrement("orders__new_empty", "id", 1000); got != "ALTER TABLE `orders__new_empty` AUTO_INCREMENT = 1000" {
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

func TestAlignAutoIncrementValue(t *testing.T) {
	// 源表删过历史行：MAX 追不上当前自增值，必须取当前自增值，避免重发已用过的 id。
	if got := AlignAutoIncrementValue(500, 1000); got != 1000 {
		t.Fatalf("应取源表当前自增值 1000，实际 %d", got)
	}
	// 正常追加：MAX+1 更大，取 MAX+1。
	if got := AlignAutoIncrementValue(1000, 800); got != 1001 {
		t.Fatalf("应取 MAX+1=1001，实际 %d", got)
	}
	// 空表且无自增计数器：从 1 开始。
	if got := AlignAutoIncrementValue(0, 0); got != 1 {
		t.Fatalf("空表应从 1 开始，实际 %d", got)
	}
}

func TestJudgeRenameSideEffects(t *testing.T) {
	if err := judgeRenameSideEffects(nil, nil); err != nil {
		t.Fatalf("没有副作用应放行: %v", err)
	}
	err := judgeRenameSideEffects(
		[]string{"trg_audit（AFTER INSERT）"},
		[]DependencyInfo{{Kind: "outbound_foreign_key", Name: "fk_user", Ref: "users"}},
	)
	if err == nil {
		t.Fatal("有触发器或外键应拒绝")
	}
	msg := err.Error()
	for _, want := range []string{"trg_audit", "fk_user", "users", "-force"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("提示信息应包含 %q，实际 %s", want, msg)
		}
	}
}

func TestJudgePurgeSamplingAllowed(t *testing.T) {
	if err := JudgePurgeSamplingAllowed(true, false); err != nil {
		t.Fatalf("抽样开启时应允许删除: %v", err)
	}
	err := JudgePurgeSamplingAllowed(false, false)
	if err == nil {
		t.Fatal("抽样关闭且未加 -force 应拒绝删除")
	}
	if !strings.Contains(err.Error(), "-force") || !strings.Contains(err.Error(), "sample_rounds") {
		t.Fatalf("拒绝信息应说明原因和出口: %s", err.Error())
	}
	if err := JudgePurgeSamplingAllowed(false, true); err != nil {
		t.Fatalf("加 -force 后应允许: %v", err)
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

func TestProgressPercent(t *testing.T) {
	cases := []struct {
		name        string
		done, total int64
		want        int
	}{
		{"零进度", 0, 100, 0},
		{"一半", 50, 100, 50},
		{"未整除向下取整", 1, 3, 33},
		{"完成", 100, 100, 100},
		{"超过总数截断到100", 130, 100, 100},
		{"总数为0视为完成", 5, 0, 100},
		{"负数不越界", -1, 100, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProgressPercent(tc.done, tc.total); got != tc.want {
				t.Fatalf("ProgressPercent(%d,%d)=%d，期望 %d", tc.done, tc.total, got, tc.want)
			}
		})
	}
}

func TestFormatProgressBar(t *testing.T) {
	cases := []struct {
		percent, width int
		want           string
	}{
		{0, 10, "[>         ]"},
		{50, 10, "[=====>    ]"},
		{100, 10, "[==========]"},
		{130, 10, "[==========]"},
		{-5, 10, "[>         ]"},
		{50, 1, "[=>]"}, // 宽度最小强制为 2
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := FormatProgressBar(tc.percent, tc.width); got != tc.want {
				t.Fatalf("FormatProgressBar(%d,%d)=%q，期望 %q", tc.percent, tc.width, got, tc.want)
			}
		})
	}
}

func TestFormatCount(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{13057038, "13,057,038"},
		{-1234567, "-1,234,567"},
	}
	for _, tc := range cases {
		if got := FormatCount(tc.in); got != tc.want {
			t.Fatalf("FormatCount(%d)=%q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	if got := FormatDuration(32 * time.Second); got != "00:32" {
		t.Fatalf("秒级格式不对: %s", got)
	}
	if got := FormatDuration(3723 * time.Second); got != "1:02:03" {
		t.Fatalf("跨小时格式不对: %s", got)
	}
	if got := FormatDuration(-time.Second); got != "00:00" {
		t.Fatalf("负数应归零: %s", got)
	}
}

func TestBuildProgressLine(t *testing.T) {
	line := BuildProgressLine(896188, 13057038, 6, 32*time.Second)
	for _, want := range []string{"搬运进度:", "6%", "896,188", "13,057,038", "00:32"} {
		if !strings.Contains(line, want) {
			t.Fatalf("进度行缺少 %q: %s", want, line)
		}
	}
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
