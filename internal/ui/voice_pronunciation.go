package ui

import (
	"regexp"
	"sort"
	"strings"
)

// Reading English technical words in a Spanish voice.
//
// A synthesizer voice carries one language's letter-to-sound rules and applies
// them to everything it is given. That is fine for prose and wrong for the
// vocabulary a coding session is made of, because a bilingual answer is Spanish
// grammar around English nouns — and Spanish rules turn those nouns into
// different words:
//
//   - "g" before e or i is /x/, so "merge" comes out "MER-je", "image" comes
//     out "i-MA-je", and "package" comes out "pa-KA-je". This is the group the
//     whole file is worth writing for: it is not an accent, it is a word the
//     listener has to decode backwards.
//   - "h" is silent, so "hash" is "as" and "hook" is "ook".
//   - "g" before a vowel needs "gu" to stay hard, so "git" is "jit".
//   - Vowel digraphs are read one letter at a time: "queue" becomes three
//     syllables, "cache" becomes "CA-che", "issue" becomes "i-SU-e".
//
// The fix is to spell the word the way a Spanish reader would have to write it
// to say the English one. It is a respelling table, not phonemes: `say` does
// accept phoneme input, but phoneme sets differ per voice and a table anyone
// can read and correct outlasts one only its author can edit.
//
// # This table was NOT verified by ear
//
// Everything else in this feature was measured — audio length is proportional
// to file size, so a shell loop answers most questions. Not this one. Whether
// "diplói" sounds more like "deploy" than "deploy" does is a judgement no file
// size reports, and the author of this table could not make it.
//
// So every entry is a starting guess, `/voice test` speaks a line that
// exercises them, and voice.pronounce in the config overrides any of them
// without a rebuild. An entry that sounds worse than what it replaced is a
// config line, not a bug report.
//
// Only Spanish has a table. The reverse direction — an English voice reading
// Spanish words — is rarer here and less damaging when it happens, because
// English rules mangle Spanish into an accent rather than into another word.
var spokenPronunciations = map[string]map[string]string{
	"es": {
		// "g" before e/i is /x/, which is the group that changes words rather
		// than accents.
		"merge":    "merch",
		"image":    "ímich",
		"images":   "ímiches",
		"package":  "pákich",
		"packages": "pákiches",
		"storage":  "stórich",
		"usage":    "iúsich",
		"message":  "mésich",
		"messages": "mésiches",
		"range":    "reinch",
		"engine":   "ényin",
		"manage":   "mánich",
		"stage":    "steich",
		"page":     "peich",
		"language": "lánguich",
		// Silent "h", and "g" that has to stay hard.
		"git":    "guit",
		"github": "guitjab",
		"hash":   "jash",
		"hook":   "juk",
		"hooks":  "juks",
		"host":   "joust",
		"header": "jéder",
		// Vowel digraphs read one letter at a time.
		"queue":    "kiú",
		"cache":    "cash",
		"issue":    "íshu",
		"issues":   "íshus",
		"deploy":   "diplói",
		"build":    "bild",
		"bug":      "bag",
		"bugs":     "bags",
		"debug":    "dibag",
		"default":  "difólt",
		"release":  "rilís",
		"feature":  "fícher",
		"update":   "apdéit",
		"timeout":  "táimaut",
		"layout":   "léiaut",
		"runtime":  "rántaim",
		"pipeline": "páiplain",
		"checkout": "chekáut",
		"rollback": "rolbák",
		"backup":   "bákap",
		"boolean":  "búlian",
		"cloud":    "claud",
		"override": "overráid",
		"workflow": "uérkflou",
		"warning":  "uórning",
		"request":  "rikuést",
		"push":     "puch",
	},
}

// isWordRune matches Go's regexp definition of a word character, which is what
// \b is a boundary between.
func isWordRune(r rune) bool {
	return r == '_' ||
		(r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z')
}

// pronunciationTable is one language's respellings, compiled once.
type pronunciationTable struct {
	pattern *regexp.Regexp
	say     map[string]string
}

// newPronunciationTable merges the configured overrides over the built-in table
// and compiles a matcher for whatever is left.
//
// An override with an empty value REMOVES an entry, which is the only way to
// say "this one sounded worse" without editing the binary.
func newPronunciationTable(language string, overrides map[string]string) *pronunciationTable {
	say := make(map[string]string, len(spokenPronunciations[language])+len(overrides))
	for word, spelling := range spokenPronunciations[language] {
		say[word] = spelling
	}
	// Sorted, so two overrides that normalise to the same key resolve the same
	// way every run. "GIT" and "git" both become "git", and map iteration order
	// would otherwise pick a different winner on different launches — a config
	// that behaves differently each time it is read is worse than one that
	// picks the wrong entry consistently. The later key alphabetically wins.
	names := make([]string, 0, len(overrides))
	for word := range overrides {
		names = append(names, word)
	}
	sort.Strings(names)
	for _, name := range names {
		word := strings.ToLower(strings.TrimSpace(name))
		if word == "" {
			continue
		}
		if spelling := strings.TrimSpace(overrides[name]); spelling == "" {
			delete(say, word)
		} else {
			say[word] = spelling
		}
	}
	if len(say) == 0 {
		return nil
	}
	// Longest first, so "packages" is not matched as "package" with a stray "s".
	words := make([]string, 0, len(say))
	for word := range say {
		words = append(words, word)
	}
	sort.Slice(words, func(i, j int) bool {
		if len(words[i]) != len(words[j]) {
			return len(words[i]) > len(words[j])
		}
		return words[i] < words[j]
	})
	// Each alternative carries its OWN boundaries, rather than one pair wrapped
	// around the whole alternation. A word boundary only exists between a word
	// character and a non-word one, so an entry ending in punctuation — an
	// override like "C++" is the obvious one somebody writes — could never
	// match with a trailing \b, and failed silently.
	alternatives := make([]string, 0, len(words))
	for _, word := range words {
		quoted := regexp.QuoteMeta(word)
		if isWordRune(rune(word[0])) {
			quoted = `\b` + quoted
		}
		if isWordRune(rune(word[len(word)-1])) {
			quoted += `\b`
		}
		alternatives = append(alternatives, quoted)
	}
	return &pronunciationTable{
		pattern: regexp.MustCompile(`(?i)(` + strings.Join(alternatives, "|") + `)`),
		say:     say,
	}
}

func (t *pronunciationTable) apply(sentence string) string {
	if t == nil || sentence == "" {
		return sentence
	}
	return t.pattern.ReplaceAllStringFunc(sentence, func(match string) string {
		if spelling := t.say[strings.ToLower(match)]; spelling != "" {
			return spelling
		}
		return match
	})
}

// pronounce respells one sentence for the voice about to read it.
//
// Per language, and cached per language, because the table is compiled from a
// regex alternation over every word in it and a turn speaks many sentences.
func (v *voiceState) pronounce(language, sentence string) string {
	if v == nil || language == "" {
		return sentence
	}
	if v.tables == nil {
		v.tables = map[string]*pronunciationTable{}
	}
	table, built := v.tables[language]
	if !built {
		table = newPronunciationTable(language, v.config.Pronounce[language])
		v.tables[language] = table
	}
	return table.apply(sentence)
}
