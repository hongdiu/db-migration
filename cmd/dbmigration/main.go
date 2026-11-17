// 命令 dbmigration 是通用数据迁移与核对工具的可执行入口。
// 具体流程由 dbmigration 库包实现，本文件仅负责解析命令行参数并调用。
package main

import (
	"dbmigration"
)

func main() {
	dbmigration.DbMigrateTool()
}
