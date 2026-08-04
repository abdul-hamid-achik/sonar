package agent

import (
	"fmt"
	"os"
	"strings"
)

// searchCoverage records what a search walk could not look at.
//
// grep, glob, and find all skip entries they cannot read — a directory they
// cannot list, a file over maxFileReadBytes, a transient I/O error — and then
// report "No matches found" with isError=false. A model consumes that as proof
// of absence and the loop records the call as a completed execution, so neither
// the user nor reconciliation ever learns the answer was partial.
//
// The triggers are ordinary repository contents, not failure conditions: an
// 8 MiB ceiling excludes lockfiles, minified bundles, SQL dumps, and large
// testdata from every search. Reporting coverage does not make those files
// searchable; it makes the negative honest.
type searchCoverage struct {
	scanned    int
	oversize   int
	unreadable int
	unlistable int
}

// scan records one entry the walk actually examined.
func (c *searchCoverage) scan() {
	if c != nil {
		c.scanned++
	}
}

// skipUnlistable records a directory or entry the walk could not enter.
func (c *searchCoverage) skipUnlistable() {
	if c != nil {
		c.unlistable++
	}
}

// skipUnreadable classifies a file the walk could not read. Size is separated
// from permission and I/O failures because it is the common case and the one a
// caller can act on by narrowing the search rather than fixing the filesystem.
func (c *searchCoverage) skipUnreadable(info os.FileInfo, limit int64) {
	if c == nil {
		return
	}
	if info != nil && info.Size() > limit {
		c.oversize++
		return
	}
	c.unreadable++
}

// complete reports whether the walk saw everything it was asked to.
func (c *searchCoverage) complete() bool {
	return c == nil || (c.oversize == 0 && c.unreadable == 0 && c.unlistable == 0)
}

// note returns a one-line qualifier for a result, or "" when coverage was
// complete. It is appended to both empty and non-empty results: a truncated
// match list built from a partial scan is as misleading as an empty one.
func (c *searchCoverage) note() string {
	if c.complete() {
		return ""
	}
	parts := make([]string, 0, 3)
	if c.oversize > 0 {
		parts = append(parts, fmt.Sprintf("%d over the %d MiB read limit",
			c.oversize, maxFileReadBytes/(1024*1024)))
	}
	if c.unreadable > 0 {
		parts = append(parts, fmt.Sprintf("%d unreadable", c.unreadable))
	}
	if c.unlistable > 0 {
		parts = append(parts, fmt.Sprintf("%d directories not listable", c.unlistable))
	}
	return fmt.Sprintf("searched %d files; skipped %s — this result is not proof of absence",
		c.scanned, strings.Join(parts, ", "))
}

// qualify appends the coverage note to a tool result.
func (c *searchCoverage) qualify(result string) string {
	note := c.note()
	if note == "" {
		return result
	}
	if strings.TrimSpace(result) == "" {
		return note
	}
	return result + "\n\n" + note
}
