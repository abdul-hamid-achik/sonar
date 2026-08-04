package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/log"
)

// NewSessionLogger creates a timestamped log file at ~/.config/sonar/logs/
// and returns a logger that writes to it. The caller should defer closing the file.
func NewSessionLogger() (*log.Logger, *os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("home dir: %w", err)
	}

	logDir := filepath.Join(home, ".config", "sonar", "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}
	if err := os.Chmod(logDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("secure log dir: %w", err)
	}

	now := time.Now()
	filename := fmt.Sprintf("%s_%06d.log", now.Format("2006-01-02_15-04-05"), now.Nanosecond()/1000)
	f, err := os.OpenFile(filepath.Join(logDir, filename), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("create log file: %w", err)
	}

	logger := log.NewWithOptions(f, log.Options{
		ReportTimestamp: true,
		TimeFormat:      time.RFC3339,
		Prefix:          "sonar",
		Level:           log.DebugLevel,
	})

	return logger, f, nil
}
