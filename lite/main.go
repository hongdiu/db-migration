// 轻量数据迁移归档工具入口。
//
// 三个动作：
//   - migrate：全表模式改名归档；条件模式分批搬到归档表（默认不删源）。
//   - verify ：独立校验，产出复查 SQL（写文件 + 打控制台），只读不删。
//   - purge  ：删除源数据，内部强制先校验，通过后才按已校验主键清单分批删。
//
// 用法：
//   dbmigration-lite -config config.yaml -action migrate
//   dbmigration-lite -config config.yaml -action verify
//   dbmigration-lite -config config.yaml -action purge
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	action := flag.String("action", "", "动作：migrate | verify | purge")
	onlyTable := flag.String("table", "", "只处理指定表（可选，默认处理配置里的全部表）")
	flag.Parse()

	if *action != "migrate" && *action != "verify" && *action != "purge" {
		return fmt.Errorf("-action 必须是 migrate、verify 或 purge")
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	db, err := OpenDB(cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	tables := selectTables(cfg.Tables, *onlyTable)
	if len(tables) == 0 {
		return fmt.Errorf("没有匹配的表（指定表名: %q）", *onlyTable)
	}

	for i := range tables {
		t := &tables[i]
		fmt.Printf("=== 表 %s，模式 %s，动作 %s ===\n", t.Name, t.Mode, *action)
		if err := processTable(ctx, db, cfg, t, *action); err != nil {
			return fmt.Errorf("表 %s 处理失败: %w", t.Name, err)
		}
	}
	fmt.Println("全部完成。")
	return nil
}

func selectTables(tables []TableCfg, only string) []TableCfg {
	if only == "" {
		return tables
	}
	for _, t := range tables {
		if t.Name == only {
			return []TableCfg{t}
		}
	}
	return nil
}

func processTable(ctx context.Context, db *sql.DB, cfg *Config, t *TableCfg, action string) error {
	switch t.Mode {
	case ModeFullTable:
		switch action {
		case "migrate":
			return RunFullTable(ctx, db, t)
		case "verify", "purge":
			return fmt.Errorf("全表模式是原子改名，不需要 verify/purge 动作；请用 -action migrate")
		}
	case ModeConditional:
		switch action {
		case "migrate":
			// 条件模式搬运：探测主键与列，读取进度后分批搬运。
			pk, err := probePrimaryKey(ctx, db, t.Name)
			if err != nil {
				return err
			}
			keyCol, err := judgePrimaryKeyUsable(t.KeyColumn, pk)
			if err != nil {
				return err
			}
			columns, err := insertableColumns(ctx, db, t.Name)
			if err != nil {
				return err
			}
			// 归档表不存在则按源表结构创建；已存在则复用（幂等搬运可续跑）。
			archiveExists, err := tableExists(ctx, db, t.ArchiveTable)
			if err != nil {
				return err
			}
			if !archiveExists {
				if _, err := db.ExecContext(ctx, BuildCreateEmptyTable(t.Name, t.ArchiveTable)); err != nil {
					return fmt.Errorf("创建归档表失败: %w", err)
				}
			}
			// 校验字段配置在搬运前就验证，避免跑完才发现字段不存在。
			if _, err := buildReviewFields(ctx, db, t.Name, t.Verify.NumericFields); err != nil {
				return err
			}
			cp, err := LoadCheckpoint(cfg.Checkpoint.Dir, t.Name)
			if err != nil {
				return err
			}
			if err := JudgeCheckpointResumable(cp, ConfigFingerprint(*t)); err != nil {
				return err
			}
		if err := RunCopy(ctx, db, cfg, t, keyCol, columns, cp); err != nil {
			return err
		}
		// 循环退出只代表游标走到头，必须立刻核对行数，避免把漏搬当成搬运完成。
		srcCount, err := countRows(ctx, db, t.Name, t.Condition)
		if err != nil {
			return fmt.Errorf("核对源表命中行数失败: %w", err)
		}
		archiveCount, err := countRows(ctx, db, t.ArchiveTable, "")
		if err != nil {
			return fmt.Errorf("核对归档表行数失败: %w", err)
		}
		if err := JudgeCopyCounts(srcCount, archiveCount); err != nil {
			return err
		}
		fmt.Printf("表 %s 搬运完成（源表命中 %d 行，归档表 %d 行），请执行 -action verify 复核。\n", t.Name, srcCount, archiveCount)
		return nil
		case "verify":
			return DoVerify(ctx, db, cfg, t)
		case "purge":
			return RunPurge(ctx, db, cfg, t)
		}
	}
	return fmt.Errorf("未知的模式或动作组合: mode=%s action=%s", t.Mode, action)
}
