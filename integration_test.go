package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestIntegrationConditionalFlow 在真实 MySQL 上验证三阶段流程。
// 需要设置环境变量 DB_MIGRATION_TEST_DSN，否则跳过。
func TestIntegrationConditionalFlow(t *testing.T) {
	dsn := os.Getenv("DB_MIGRATION_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 DB_MIGRATION_TEST_DSN，跳过集成测试")
	}
	cfg := &DBConfigV2{DSN: dsn, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifeSeconds: 3600}
	db, err := OpenDB(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	table := "it_migrate_src"
	histTable := table + "_hist"
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS "+histTable)
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS "+table)
	_, _ = db.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE %s (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		amount DECIMAL(20,2) NOT NULL,
		fee DOUBLE,
		note VARCHAR(64),
		created_at DATETIME NOT NULL
	) ENGINE=InnoDB`, table))
	for i := 0; i < 50; i++ {
		_, err := db.ExecContext(ctx, fmt.Sprintf(
			"INSERT INTO %s (amount, fee, note, created_at) VALUES (?, ?, ?, ?)", table),
			float64(i)+0.01, float64(i)*0.1, fmt.Sprintf("n%d", i), time.Now().AddDate(-1, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
	}
	defer db.ExecContext(ctx, "DROP TABLE IF EXISTS "+table)
	defer db.ExecContext(ctx, "DROP TABLE IF EXISTS "+histTable)

	timestamp := false
	tc := &TableConfig{
		Name: table, Mode: "conditional", Condition: "id > 0", HistorySuffix: "_hist",
		HistoryTimestamp: &timestamp, KeyColumn: "id", BatchSize: 10, RetentionDays: 365,
		Verify: VerifyConfig{
			RowCount: boolPtr(true), NumericSumFields: []string{"amount", "fee"},
			SampleRounds: 2, SampleSize: 5, AbsTolerance: 1e-6, RelTolerance: 1e-9,
		},
	}
	mcfg := &MigrateConfig{Output: OutputConfig{Dir: t.TempDir()}, Checkpoint: CheckpointConfig{Dir: t.TempDir()}}

	// 复制阶段。
	rep := runConditionalCopy(ctx, db, tc, mcfg, Options{}, "stamp")
	if rep.Err != nil || !rep.Pass() {
		t.Fatalf("复制阶段未通过: err=%v checks=%+v diffs=%+v", rep.Err, rep.Checks, rep.Diffs)
	}
	srcCount, _ := countRows(ctx, db, table, "")
	if srcCount != 50 {
		t.Fatalf("复制后源表应仍有50行，实际 %d", srcCount)
	}

	// 清理阶段。
	rep2 := runConditionalPurge(ctx, db, tc, mcfg)
	if rep2.Err != nil || !rep2.Pass() {
		t.Fatalf("清理阶段未通过: err=%v checks=%+v", rep2.Err, rep2.Checks)
	}
	srcCount, _ = countRows(ctx, db, table, "")
	if srcCount != 0 {
		t.Fatalf("清理后源表应为0行，实际 %d", srcCount)
	}

	// 清理成功后登记状态必须转为 purged，否则该批历史表永远停在 active，
	// 既无法被再次清理，也无法进入保留期删除流程。
	entries, err := listHistory(ctx, db, table)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || entries[len(entries)-1].Status != statusPurged {
		t.Fatalf("清理成功后历史表状态应为 purged，实际 %+v", entries)
	}

	// 历史表数据必须完好：清理只删源表，归档数据一条不能少。
	histCount, _ := countRows(ctx, db, histTable, "")
	if histCount != 50 {
		t.Fatalf("清理后历史表应仍有50行，实际 %d", histCount)
	}
}
