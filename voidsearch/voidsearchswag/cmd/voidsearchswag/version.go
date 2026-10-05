package main

import (
	"fmt"
)

// cmdVersion печатает версию. Версия - такой же ответ, как у остальных команд: до
// правки скрипт, который собирал диагностику со всех команд в JSON, получал здесь
// текст «voidsearchswag 0.1.0» (rc=0, 46 байт) и падал на разборе. Флаг
// разбирается по оставшимся аргументам, потому что своего FlagSet у version нет и
// не нужен: других флагов команда не принимает.
func cmdVersion(rest []string) {
	if wantsJSON(rest) {
		writeJSON(map[string]any{
			"program": "voidsearchswag",
			"version": version,
			"commit":  buildCommit,
			"date":    buildDate,
		})
		return
	}
	line := "voidsearchswag " + version
	if buildCommit != "unknown" || buildDate != "unknown" {
		line += " (" + buildCommit + ", " + buildDate + ")"
	}
	fmt.Println(line)
}
