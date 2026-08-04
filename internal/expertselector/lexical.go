package expertselector

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenSet map[string]struct{}

func validatePrompt(ctx context.Context, value string) error {
	nextCheck := 0
	for index := 0; index < len(value); {
		if index >= nextCheck {
			if err := ctx.Err(); err != nil {
				return err
			}
			nextCheck = index + 4096
		}
		character, size := utf8.DecodeRuneInString(value[index:])
		if character == utf8.RuneError && size == 1 {
			return ErrInvalidPrompt
		}
		index += size
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

var lexicalStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "as": {}, "at": {}, "be": {}, "by": {}, "for": {}, "from": {},
	"in": {}, "into": {}, "is": {}, "it": {}, "of": {}, "on": {}, "or": {}, "the": {}, "this": {},
	"to": {}, "use": {}, "using": {}, "with": {},
	"de": {}, "del": {}, "el": {}, "en": {}, "la": {}, "las": {}, "los": {}, "o": {}, "para": {},
	"por": {}, "un": {}, "una": {}, "usar": {}, "y": {},
	"agent": {}, "analyze": {}, "analysis": {}, "expert": {}, "help": {}, "profile": {},
	"review": {}, "specialist": {}, "specialized": {}, "support": {}, "task": {}, "tool": {}, "work": {},
}

// lexicalFamilies add a small, host-owned semantic layer without consulting a
// model. Both prompts and profile contracts receive the same canonical terms.
var lexicalFamilies = map[string][]string{
	// Media and common file extensions.
	"video": {"video", "media"}, "videos": {"video", "media"},
	"mp4": {"video", "media"}, "mov": {"video", "media"}, "mkv": {"video", "media"},
	"webm": {"video", "media"}, "avi": {"video", "media"}, "mpeg": {"video", "media"},
	"image": {"image", "media"}, "images": {"image", "media"}, "imagen": {"image", "media"},
	"jpg": {"image", "media"}, "jpeg": {"image", "media"}, "png": {"image", "media"},
	"gif": {"image", "media"}, "webp": {"image", "media"}, "screenshot": {"image", "media", "ui"},
	"audio": {"audio", "media"}, "mp3": {"audio", "media"}, "wav": {"audio", "media"},
	"m4a": {"audio", "media"}, "flac": {"audio", "media"}, "transcribe": {"audio", "transcription"},
	"document": {"document"}, "documents": {"document"}, "documento": {"document"},
	"pdf": {"document"}, "doc": {"document"}, "docx": {"document"},

	// Security.
	"auth": {"authentication", "security"}, "authentication": {"authentication", "security"},
	"authorization": {"authorization", "security"}, "security": {"security"}, "secure": {"security"},
	"vulnerability": {"vulnerability", "security"}, "vulnerabilities": {"vulnerability", "security"},
	"credential": {"credential", "security"}, "credentials": {"credential", "security"},
	"seguridad": {"security"}, "autenticacion": {"authentication", "security"},

	// UX, UI, TUI, and accessibility.
	"ux": {"ux", "ui"}, "ui": {"ui"}, "tui": {"tui", "ui"},
	"interface": {"ui"}, "interfaces": {"ui"}, "usability": {"ux"}, "usabilidad": {"ux"},
	"accessibility": {"accessibility", "ux"}, "accessible": {"accessibility", "ux"},
	"accesibilidad": {"accessibility", "ux"}, "keyboard": {"keyboard", "accessibility"},

	// Common engineering domains.
	"database": {"database"}, "postgres": {"database", "postgres"}, "sqlite": {"database", "sqlite"},
	"frontend": {"frontend", "ui"}, "backend": {"backend"}, "api": {"api", "backend"},
	"test": {"testing"}, "tests": {"testing"}, "testing": {"testing"}, "verify": {"verification"},
	"verification": {"verification"}, "debug": {"debugging"}, "diagnose": {"debugging"},
}

func boundedPrompt(value string) string {
	if len(value) <= maxPromptLexicalBytes {
		return value
	}
	half := maxPromptLexicalBytes / 2
	head := safePrefix(value, half)
	tail := safeSuffix(value, half)
	return head + " " + tail
}

func safePrefix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func safeSuffix(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	start := len(value) - limit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return value[start:]
}

func tokenize(ctx context.Context, value string, limit int) (tokenSet, error) {
	result := make(tokenSet)
	var token strings.Builder
	flush := func() bool {
		if token.Len() == 0 {
			return len(result) >= limit
		}
		word := token.String()
		token.Reset()
		addLexicalToken(result, word)
		return len(result) >= limit
	}

	processed := 0
	for _, character := range value {
		processed++
		if processed&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		character = normalizeLexicalRune(character)
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			// A maliciously long path component or word cannot grow memory
			// without bound. Delimited extensions such as .mp4 remain visible.
			if token.Len() < 64 {
				token.WriteRune(unicode.ToLower(character))
			}
			continue
		}
		if flush() {
			break
		}
	}
	if len(result) < limit {
		flush()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func addLexicalToken(result tokenSet, value string) {
	if value == "" {
		return
	}
	if _, stop := lexicalStopWords[value]; !stop {
		result[value] = struct{}{}
	}
	for _, canonical := range lexicalFamilies[value] {
		if _, stop := lexicalStopWords[canonical]; !stop {
			result[canonical] = struct{}{}
		}
	}
}

func normalizeLexicalRune(value rune) rune {
	switch value {
	case 'á', 'à', 'ä', 'â', 'ã', 'å', 'Á', 'À', 'Ä', 'Â', 'Ã', 'Å':
		return 'a'
	case 'é', 'è', 'ë', 'ê', 'É', 'È', 'Ë', 'Ê':
		return 'e'
	case 'í', 'ì', 'ï', 'î', 'Í', 'Ì', 'Ï', 'Î':
		return 'i'
	case 'ó', 'ò', 'ö', 'ô', 'õ', 'Ó', 'Ò', 'Ö', 'Ô', 'Õ':
		return 'o'
	case 'ú', 'ù', 'ü', 'û', 'Ú', 'Ù', 'Ü', 'Û':
		return 'u'
	case 'ñ', 'Ñ':
		return 'n'
	case 'ç', 'Ç':
		return 'c'
	default:
		return value
	}
}

func unionTokens(left, right tokenSet) tokenSet {
	result := make(tokenSet, len(left)+len(right))
	for value := range left {
		result[value] = struct{}{}
	}
	for value := range right {
		result[value] = struct{}{}
	}
	return result
}

func intersectionCount(left, right tokenSet) int {
	if len(left) > len(right) {
		left, right = right, left
	}
	count := 0
	for value := range left {
		if _, ok := right[value]; ok {
			count++
		}
	}
	return count
}

func equivalentToAny(value candidate, selected []candidate) bool {
	for _, existing := range selected {
		if jaccardPercent(value.contractTokens, existing.contractTokens) >= 90 {
			return true
		}
	}
	return false
}

func maximumSimilarity(value tokenSet, selected []candidate) int {
	maximum := 0
	for _, existing := range selected {
		current := jaccardPercent(value, existing.contractTokens)
		if current > maximum {
			maximum = current
		}
	}
	return maximum
}

func jaccardPercent(left, right tokenSet) int {
	if len(left) == 0 && len(right) == 0 {
		return 100
	}
	intersection := intersectionCount(left, right)
	union := len(left) + len(right) - intersection
	if union == 0 {
		return 100
	}
	return intersection * 100 / union
}
