package main

import (
	"strings"
	"testing"
)

func TestJudgePurgeProgress(t *testing.T) {
	const fp = "fp123"

	// 没有进度或不在删除中：从头开始，不算续跑。
	p, err := JudgePurgeProgress(nil, fp)
	if err != nil || p.Resuming || p.StartKey != 0 {
		t.Fatalf("无进度应从头开始: %+v err=%v", p, err)
	}
	p, err = JudgePurgeProgress(&Checkpoint{Fingerprint: fp, Stage: "copied", LastKey: 900}, fp)
	if err != nil || p.Resuming || p.StartKey != 0 {
		t.Fatalf("搬运阶段不应触发删除续跑: %+v err=%v", p, err)
	}

	// 删除中断：按进度续跑。
	cp := &Checkpoint{Fingerprint: fp, Stage: "purging", PurgeLastKey: 5000, PurgeDeleted: 100}
	p, err = JudgePurgeProgress(cp, fp)
	if err != nil || !p.Resuming || p.StartKey != 5000 || p.DeletedBefore != 100 {
		t.Fatalf("删除中断应可续跑: %+v err=%v", p, err)
	}

	// 配置变了：拒绝拿旧进度删新数据。
	if _, err := JudgePurgeProgress(cp, "other"); err == nil {
		t.Fatal("指纹不一致应拒绝续跑")
	}
	// 内容异常：拒绝续跑。
	bad := &Checkpoint{Fingerprint: fp, Stage: "purging", PurgeLastKey: -1}
	if _, err := JudgePurgeProgress(bad, fp); err == nil {
		t.Fatal("负数进度应拒绝续跑")
	}
}

func TestApplyPurgeBatchKeepsConservation(t *testing.T) {
	// 初始：源表待删 100，归档 100，已删 0。逐批删 50/30/20，守恒式应始终成立。
	src := int64(100)
	var deleted int64
	const archive int64 = 100
	for _, affected := range []int64{50, 30, 20} {
		if err := JudgePurgeAllowed(PurgeVerdict{
			SrcRowsToDelete: src, DeletedSoFar: deleted, ArchiveRows: archive, BatchRows: affected,
		}); err != nil {
			t.Fatalf("批次 %d 守恒判定失败: %v", affected, err)
		}
		src, deleted = ApplyPurgeBatch(src, deleted, affected)
	}
	if src != 0 || deleted != archive {
		t.Fatalf("删完后计数应归位: src=%d deleted=%d", src, deleted)
	}
}

func TestPurgeSQLBuilders(t *testing.T) {
	q := BuildNewWriteCheckQuery("orders", "created_at < '2024-01-01'", "id", 9000000)
	if !strings.Contains(q, "`id` > 9000000") {
		t.Fatalf("新写入检查应带主键上界: %s", q)
	}
	if !strings.Contains(q, "(created_at < '2024-01-01')") {
		t.Fatalf("新写入检查应带迁移条件: %s", q)
	}
	if !strings.Contains(q, "LIMIT 1") {
		t.Fatalf("新写入检查应命中即停: %s", q)
	}

	kq := BuildArchivedKeysQuery("orders_archive", "id", 5000, 500)
	if !strings.Contains(kq, "`id` > 5000") || !strings.Contains(kq, "LIMIT 500") {
		t.Fatalf("续跑取主键应带起点和批大小: %s", kq)
	}
	if !strings.Contains(kq, "ORDER BY `id`") {
		t.Fatalf("取主键必须有序: %s", kq)
	}
}

func TestBuildSampleWhereByKey(t *testing.T) {
	if BuildSampleWhereByKey("id", 0) != "" || BuildSampleWhereByKey("id", -1) != "" {
		t.Fatal("起点为 0 或负数时不应加过滤条件")
	}
	got := BuildSampleWhereByKey("id", 5000)
	if got != "`id` > 5000" {
		t.Fatalf("续跑过滤条件不对: %s", got)
	}
	if sampleWhere(got) != " WHERE (`id` > 5000)" {
		t.Fatalf("WHERE 片段拼装不对: %s", sampleWhere(got))
	}
	if sampleWhere("") != "" {
		t.Fatal("空条件不应产生 WHERE")
	}
}

func TestReviewSQLWithArchiveCondition(t *testing.T) {
	fields := []ReviewField{{Name: "amount", Kind: NumericInteger}}
	results := []ReviewItemResult{{Label: "记录数", Verdict: "通过", Detail: "两侧均为 10 行"}}

	limited := BuildReviewSQLWithArchiveCondition("orders", "orders_archive", "created_at < '2024-01-01'",
		"`id` > 5000", "id", fields, results, nil, 0.0001, 0.00001)
	if !strings.Contains(limited, "归档表校验范围") {
		t.Fatalf("续跑复查 SQL 应注明范围: %s", limited)
	}
	for _, want := range []string{
		"SELECT COUNT(*) AS 归档表行数 FROM `orders_archive` WHERE (`id` > 5000)",
		"SELECT SUM(`amount`) AS 归档表求和 FROM `orders_archive` WHERE (`id` > 5000)",
		"SELECT COUNT(`amount`) AS 归档表非空数 FROM `orders_archive` WHERE (`id` > 5000)",
	} {
		if !strings.Contains(limited, want) {
			t.Fatalf("复查 SQL 缺少 %q", want)
		}
	}

	// 全量校验时不应出现范围限制，保持原有输出不变。
	plain := BuildReviewSQL("orders", "orders_archive", "", "id", fields, results, nil, 0.0001, 0.00001)
	if strings.Contains(plain, "`id` >") || strings.Contains(plain, "归档表校验范围") {
		t.Fatalf("全量复查 SQL 不应带续跑范围: %s", plain)
	}
}
