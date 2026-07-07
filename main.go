package main

import (
	// 嵌入 IANA 时区数据库，保证在无 zoneinfo 的精简容器里
	// time.LoadLocation(如统计时区 stats_timezone)也能解析。
	_ "time/tzdata"

	"github.com/bestruirui/octopus/cmd"
)

func main() {
	cmd.Execute()
}
