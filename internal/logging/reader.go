package logging

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// LogEntry describes a single log file on disk.
type LogEntry struct {
	Path    string
	ModTime time.Time
	Size    int64
}

// LogDir returns the directory where session logs are stored.
func LogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "sonar", "logs")
}

// ListLogs returns the n most recent log files sorted newest-first.
// If n <= 0 all entries are returned.
func ListLogs(n int) ([]LogEntry, error) {
	return listLogsIn(LogDir(), n)
}

// listLogsIn is the testable core: it reads from an arbitrary directory.
func listLogsIn(dir string, n int) ([]LogEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []LogEntry{}, nil
		}
		return nil, fmt.Errorf("read log dir: %w", err)
	}

	var logs []LogEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		logs = append(logs, LogEntry{
			Path:    filepath.Join(dir, e.Name()),
			ModTime: info.ModTime(),
			Size:    info.Size(),
		})
	}

	// Sort newest first.
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].ModTime.After(logs[j].ModTime)
	})

	if n > 0 && n < len(logs) {
		logs = logs[:n]
	}
	return logs, nil
}

// LatestLogPath returns the path to the most recently modified log file.
func LatestLogPath() (string, error) {
	return latestLogPathIn(LogDir())
}

// latestLogPathIn is the testable core.
func latestLogPathIn(dir string) (string, error) {
	logs, err := listLogsIn(dir, 1)
	if err != nil {
		return "", err
	}
	if len(logs) == 0 {
		return "", fmt.Errorf("no log files found in %s", dir)
	}
	return logs[0].Path, nil
}

// TailLog reads the last n lines of the file at path.
// If the file has fewer than n lines, all lines are returned.
func TailLog(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer func() { _ = f.Close() }()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close log: %w", err)
	}

	if n > 0 && n < len(lines) {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
