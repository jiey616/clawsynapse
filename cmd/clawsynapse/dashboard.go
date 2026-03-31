package main

import (
	"io"

	"clawsynapse/internal/tui/dashboard"
)

func runDashboard(args []string, stdout, stderr io.Writer) error {
	logSource := "journalctl -u clawsynapsed.service"
	if serviceGOOS == "darwin" {
		logSource = "~/.clawsynapse/log/*.log"
	}

	return dashboard.Run(args, stdout, stderr, dashboard.Options{
		Version:   version,
		LogSource: logSource,
		LogLines:  defaultLogLines,
		Logs:      defaultLogProvider{runner: execServiceRunner{}},
	})
}
