// Package docs compares the published site against the harness it describes.
//
// A `docs/` tree lived here once and was deleted, because every page described
// a different product and told the reader to install it. AGENTS.md recorded the
// rule that came out of that: wrong documentation is worse than none, so if a
// doc tree returns it is written for sonar or it does not ship.
//
// This is what makes "written for sonar" checkable rather than a promise.
// Nothing compares two prose files — that is the same reason internal/drift
// exists — so the site's claims are compared against the code they claim
// something about. A page that names a slash command sonar does not have, or a
// configuration key VoiceConfig does not define, fails the Go suite in the same
// run as everything else.
//
// It deliberately does not check prose for accuracy. No test can. What it can
// check is the class of error that actually happened last time: a page naming a
// thing that is simply not there.
package docs

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/command"
	"github.com/abdul-hamid-achik/sonar/internal/config"
)

// docsRoot is where the site's pages live, relative to this package.
const docsRoot = "../../docs/src/content/docs"

// slashCommand matches a command as the site writes one: in backticks, or at
// the start of a line inside a fenced example.
//
// The backtick-only version was a false negative on most of the site, because
// most pages show commands in fenced blocks — the guard was checking the least
// common way the docs name a thing.
var slashCommand = regexp.MustCompile("(?m)(?:`|^)/([a-z][a-z0-9-]*)")

// configKey matches a dotted setting in backticks — `voice.speak_when`.
var configKey = regexp.MustCompile("`([a-z][a-z0-9_]*(?:\\.[a-z][a-z0-9_]*)+)`")

func TestTheSiteOnlyNamesCommandsThatExist(t *testing.T) {
	registry := command.NewRegistry()
	command.RegisterBuiltins(registry)
	known := map[string]bool{}
	for _, entry := range registry.All() {
		known[entry.Name] = true
		for _, alias := range entry.Aliases {
			known[alias] = true
		}
	}
	if len(known) == 0 {
		t.Fatal("read no commands from the registry; this test cannot prove anything")
	}

	forEachPage(t, func(page string, body string) {
		for _, match := range slashCommand.FindAllStringSubmatch(body, -1) {
			if !known[match[1]] {
				t.Errorf("%s names /%s, which is not a command sonar has", page, match[1])
			}
		}
	})
}

func TestTheSiteOnlyNamesSettingsThatExist(t *testing.T) {
	known := configKeys(reflect.TypeOf(config.Config{}), "")
	if len(known) == 0 {
		t.Fatal("read no settings from the config type; this test cannot prove anything")
	}

	forEachPage(t, func(page string, body string) {
		for _, match := range configKey.FindAllStringSubmatch(body, -1) {
			key := match[1]
			if known[key] {
				continue
			}
			// A dotted lowercase token in backticks is usually a setting and
			// sometimes a filename or a package path, so the first segment has
			// to look like a section for this to be a claim about sonar.
			//
			// It used to require the section to EXIST, which made an unknown
			// section its own excuse: `bogus.option` passed because nothing
			// called "bogus" was there to contradict it. Now a token that is
			// not a known key and not a known file extension is reported, and
			// the extensions are named rather than inferred.
			if suffix := key[strings.LastIndex(key, ".")+1:]; fileExtensions[suffix] {
				continue
			}
			t.Errorf("%s names %s, which is not a setting sonar has", page, key)
		}
	})
}

// fileExtensions are the dotted tokens that are a filename rather than a
// setting. Named rather than inferred: "anything with an unknown section is
// probably a file" is exactly the reasoning that let a wrong setting pass.
var fileExtensions = map[string]bool{
	"yaml": true, "yml": true, "md": true, "mdx": true, "go": true,
	"json": true, "toml": true, "sh": true, "txt": true, "xml": true,
}

// configKeys walks the config struct and collects every yaml path it defines.
func configKeys(structType reflect.Type, prefix string) map[string]bool {
	keys := map[string]bool{}
	if structType == nil || structType.Kind() != reflect.Struct {
		return keys
	}
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		keys[path] = true
		nested := field.Type
		for nested.Kind() == reflect.Pointer || nested.Kind() == reflect.Slice {
			nested = nested.Elem()
		}
		for key := range configKeys(nested, path) {
			keys[key] = true
		}
	}
	return keys
}

func forEachPage(t *testing.T, check func(page, body string)) {
	t.Helper()
	root, err := filepath.Abs(docsRoot)
	if err != nil {
		t.Fatalf("resolve the docs root: %v", err)
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		// Not a skip. The site is committed to this repository, so its absence
		// is a broken checkout rather than an optional feature — and a guard
		// that reports green when there is nothing to guard is worse than no
		// guard, because it looks like coverage.
		t.Fatalf("no docs tree at %s; the site is part of this repository", root)
	}
	pages := 0
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		switch filepath.Ext(path) {
		case ".md", ".mdx":
		default:
			return nil
		}
		body, readErr := os.ReadFile(path) // #nosec G304 -- repository-local docs
		if readErr != nil {
			return readErr
		}
		pages++
		relative, _ := filepath.Rel(root, path)
		check(relative, string(body))
		return nil
	})
	if walkErr != nil {
		t.Fatalf("read the docs tree: %v", walkErr)
	}
	if pages == 0 {
		t.Fatal("found no pages; this test would pass on an empty site")
	}
}
