package agent

import (
	"fmt"

	"github.com/camptocamp/bivac/internal/engines"
	"github.com/camptocamp/bivac/internal/utils"
)

func Backup(targetURL, backupPath, hostname string) {
	e := &engines.ResticEngine{
		DefaultArgs: []string{
			"--no-cache",
			"--json",
			"-r",
			targetURL,
		},
		Output: make(map[string]utils.OutputFormat),
	}

	output := e.Backup(backupPath, hostname)
	fmt.Println(output)
	return
}

func Restore(targetURL, backupPath, hostname string) {
	return
}