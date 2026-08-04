package agent

import (
	"context"
	"fmt"
	"strings"
)

func (a *Agent) handleDiff(ctx context.Context, args map[string]any) (string, bool) {
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("error: diff cancelled: %v", err), true
	}
	path, _ := args["path"].(string)
	newContent, _ := args["new_content"].(string)

	if path == "" {
		return "error: path is required", true
	}
	if newContent == "" {
		return "error: new_content is required", true
	}

	readable, err := a.resolveReadablePath(path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = readable.close() }()

	// Read current content
	oldContent, err := readable.readBounded(maxFileReadBytes)
	if err != nil {
		return fmt.Sprintf("error reading file: %v", err), true
	}

	oldLines := strings.Split(string(oldContent), "\n")
	newLines := strings.Split(newContent, "\n")
	const maxDiffCells = 4_000_000
	if len(newLines) > 0 && len(oldLines) > maxDiffCells/len(newLines) {
		return fmt.Sprintf("error: diff is too large (%d x %d lines); compare a narrower region or use an approved git diff command", len(oldLines), len(newLines)), true
	}

	// Compute diff using simple line-by-line comparison
	diff, err := computeDiff(ctx, oldLines, newLines)
	if err != nil {
		return fmt.Sprintf("error: diff cancelled: %v", err), true
	}

	if diff == "" {
		return "No changes (files are identical)", false
	}

	return diff, false
}

// computeDiff produces a unified diff-like output between old and new lines.
func computeDiff(ctx context.Context, oldLines, newLines []string) (string, error) {
	var result strings.Builder

	oldLen := len(oldLines)
	newLen := len(newLines)

	// Simple diff algorithm: find longest common subsequence
	lcs, err := longestCommonSubsequence(ctx, oldLines, newLines)
	if err != nil {
		return "", err
	}

	oldIdx := 0
	newIdx := 0
	lcsIdx := 0

	for oldIdx < oldLen || newIdx < newLen {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if lcsIdx < len(lcs) {
			// Print removed lines from old
			for oldIdx < oldLen && oldLines[oldIdx] != lcs[lcsIdx] {
				fmt.Fprintf(&result, "-%s\n", oldLines[oldIdx])
				oldIdx++
			}
			// Print added lines from new
			for newIdx < newLen && newLines[newIdx] != lcs[lcsIdx] {
				fmt.Fprintf(&result, "+%s\n", newLines[newIdx])
				newIdx++
			}
			// Print common line
			if oldIdx < oldLen && newIdx < newLen {
				fmt.Fprintf(&result, " %s\n", lcs[lcsIdx])
				oldIdx++
				newIdx++
				lcsIdx++
			}
		} else {
			// No more LCS - print remaining lines
			for oldIdx < oldLen {
				fmt.Fprintf(&result, "-%s\n", oldLines[oldIdx])
				oldIdx++
			}
			for newIdx < newLen {
				fmt.Fprintf(&result, "+%s\n", newLines[newIdx])
				newIdx++
			}
		}
	}

	return result.String(), nil
}

// longestCommonSubsequence finds the LCS of two string slices.
func longestCommonSubsequence(ctx context.Context, a, b []string) ([]string, error) {
	m, n := len(a), len(b)
	// DP table
	dp := make([][]int, m+1)
	for i := range dp {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dp[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else {
				if dp[i-1][j] > dp[i][j-1] {
					dp[i][j] = dp[i-1][j]
				} else {
					dp[i][j] = dp[i][j-1]
				}
			}
		}
	}

	// Backtrack to find LCS
	var lcs []string
	i, j := m, n
	for i > 0 && j > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if a[i-1] == b[j-1] {
			lcs = append([]string{a[i-1]}, lcs...)
			i--
			j--
		} else if dp[i-1][j] > dp[i][j-1] {
			i--
		} else {
			j--
		}
	}

	return lcs, nil
}
