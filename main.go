// 通用数据迁移与核对工具的可执行入口。
//
// 根目录即主包，直接 `go build .` 或 `go run .` 即可，不再需要 cmd 子目录。
// 具体流程在 migrate.go / engine.go / history.go 等文件里实现。
package main

func main() {
	DbMigrateTool()
}
