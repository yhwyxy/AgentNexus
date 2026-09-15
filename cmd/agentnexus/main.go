// 可执行程序入口
package main

import (
	"flag"
	"log"

	"github.com/yhwyxy/AgentNexus/internal/app"
)

func main() {
	configPath := flag.String(
		"config",
		"",
		"path to AgentNexus configuration file",
	)
	flag.Parse()

	if err := app.Run(*configPath); err != nil {
		log.Fatal(err)
	}
}
