// Package expertselector performs local, deterministic expert selection for
// application-level Team, Swarm, and mixture-of-experts orchestration.
//
// Selection is pure with respect to external systems: it never calls a model,
// tool, registry, network service, or filesystem. The prompt is reduced to a
// bounded lexical token set and is never copied into reasons or errors.
package expertselector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	DefaultMaxExperts    = 3
	MaxSelectedExperts   = 16
	MaxCandidateProfiles = 256
	MaxPromptBytes       = 4 * 1024 * 1024
	MaxReasonBytes       = 192

	maxProfileNameBytes   = 128
	maxDescriptionBytes   = 8 * 1024
	maxModelBytes         = 256
	maxUseCases           = 64
	maxUseCaseBytes       = 512
	maxPromptLexicalBytes = 64 * 1024
	maxPromptTokens       = 512
	maxProfileTokens      = 512
)

// Strategy controls the application-level selection policy.
type Strategy string

const (
	StrategyTeam  Strategy = "team"
	StrategySwarm Strategy = "swarm"
	StrategyMoE   Strategy = "moe"
)

var (
	ErrInvalidStrategy        = errors.New("expertselector: invalid strategy")
	ErrInvalidContext         = errors.New("expertselector: nil context")
	ErrInvalidPrompt          = errors.New("expertselector: prompt is not valid UTF-8")
	ErrPromptLimit            = errors.New("expertselector: prompt exceeds the bounded input limit")
	ErrNoProfiles             = errors.New("expertselector: no valid profiles")
	ErrProfileLimit           = errors.New("expertselector: too many candidate profiles")
	ErrInvalidProfile         = errors.New("expertselector: invalid or unbounded profile")
	ErrUnknownExplicitProfile = errors.New("expertselector: explicit profile not found")
	ErrExpertLimit            = errors.New("expertselector: expert limit is invalid or excludes an explicit profile")
	ErrNoMatch                = errors.New("expertselector: no semantic match and no explicit fallback")
)

// Profile is a bounded application-owned expert contract. UseCases and
// Description are the only fields consulted by semantic routing; Name and
// Model are identifiers returned to the caller.
type Profile struct {
	Name        string
	Description string
	UseCases    []string
	Model       string
}

// Options carries selection authority. ExplicitNames are case-insensitive and
// deduplicated in first-seen order.
//
// Team selects exactly those names when provided. Swarm treats them as
// required seeds before adding diverse experts. MoE uses them only as an
// explicit fallback when no profile has a positive lexical match.
type Options struct {
	ExplicitNames []string
	MaxExperts    int
}

// Request is the complete local selector input.
type Request struct {
	Strategy Strategy
	Prompt   string
	Profiles []Profile
	Options  Options
}

// Selection is one ordered expert choice. Reason is host-authored, bounded,
// and contains no prompt text, path, filename, or arbitrary profile prose.
// Score is always in the inclusive range 0..100.
type Selection struct {
	Profile Profile
	Reason  string
	Score   int
}

type candidate struct {
	profile           Profile
	key               string
	index             int
	useTokens         tokenSet
	descriptionTokens tokenSet
	contractTokens    tokenSet
	matchScore        int
	matchedSignals    int
}

// Select chooses experts without consulting external state. Results are
// deterministic for the same request, including tie breaks. The input slices
// are never mutated and selected profiles are deep copies.
func Select(ctx context.Context, request Request) ([]Selection, error) {
	if ctx == nil {
		return nil, ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Strategy != StrategyTeam && request.Strategy != StrategySwarm && request.Strategy != StrategyMoE {
		return nil, ErrInvalidStrategy
	}
	if len(request.Prompt) > MaxPromptBytes {
		return nil, ErrPromptLimit
	}
	if err := validatePrompt(ctx, request.Prompt); err != nil {
		return nil, err
	}

	profiles, byName, err := prepareProfiles(ctx, request.Profiles)
	if err != nil {
		return nil, err
	}
	explicit, err := resolveExplicitNames(request.Options.ExplicitNames, byName)
	if err != nil {
		return nil, err
	}
	limit, err := resolveLimit(request.Options.MaxExperts, len(profiles), len(explicit))
	if err != nil {
		return nil, err
	}

	promptTokens, err := tokenize(ctx, boundedPrompt(request.Prompt), maxPromptTokens)
	if err != nil {
		return nil, err
	}
	for index := range profiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		profiles[index].matchScore, profiles[index].matchedSignals = lexicalMatch(promptTokens, profiles[index])
	}

	switch request.Strategy {
	case StrategyTeam:
		return selectTeam(profiles, explicit, limit), nil
	case StrategySwarm:
		return selectSwarm(ctx, profiles, explicit, limit)
	case StrategyMoE:
		return selectMoE(ctx, profiles, explicit, limit)
	default:
		return nil, ErrInvalidStrategy
	}
}

func prepareProfiles(ctx context.Context, values []Profile) ([]candidate, map[string]int, error) {
	if len(values) == 0 {
		return nil, nil, ErrNoProfiles
	}
	if len(values) > MaxCandidateProfiles {
		return nil, nil, ErrProfileLimit
	}
	candidates := make([]candidate, 0, min(len(values), MaxSelectedExperts*4))
	byName := make(map[string]int, len(values))
	for index, value := range values {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		profile, err := validateAndCloneProfile(value)
		if err != nil {
			return nil, nil, err
		}
		key := profileKey(profile.Name)
		if _, duplicate := byName[key]; duplicate {
			// Configuration can merge profiles from multiple scopes. First-seen
			// wins deterministically and case variants cannot create two experts.
			continue
		}
		useText := strings.Join(profile.UseCases, " ")
		useTokens, err := tokenize(ctx, useText, maxProfileTokens)
		if err != nil {
			return nil, nil, err
		}
		descriptionTokens, err := tokenize(ctx, profile.Description, maxProfileTokens)
		if err != nil {
			return nil, nil, err
		}
		contractTokens := unionTokens(useTokens, descriptionTokens)
		byName[key] = len(candidates)
		candidates = append(candidates, candidate{
			profile: profile, key: key, index: index,
			useTokens: useTokens, descriptionTokens: descriptionTokens, contractTokens: contractTokens,
		})
	}
	if len(candidates) == 0 {
		return nil, nil, ErrNoProfiles
	}
	return candidates, byName, nil
}

func validateAndCloneProfile(value Profile) (Profile, error) {
	if !boundedSingleLine(value.Name, maxProfileNameBytes, false) || strings.ContainsAny(value.Name, "/\\") {
		return Profile{}, ErrInvalidProfile
	}
	if !boundedText(value.Description, maxDescriptionBytes) || !boundedSingleLine(value.Model, maxModelBytes, true) {
		return Profile{}, ErrInvalidProfile
	}
	if len(value.UseCases) > maxUseCases {
		return Profile{}, ErrInvalidProfile
	}
	useCases := make([]string, len(value.UseCases))
	for index, useCase := range value.UseCases {
		if !boundedText(useCase, maxUseCaseBytes) {
			return Profile{}, ErrInvalidProfile
		}
		useCases[index] = useCase
	}
	return Profile{
		Name: value.Name, Description: value.Description,
		UseCases: useCases, Model: value.Model,
	}, nil
}

func boundedSingleLine(value string, maxBytes int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || len(value) > maxBytes || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return false
	}
	if value == "" {
		return allowEmpty
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func boundedText(value string, maxBytes int) bool {
	if !utf8.ValidString(value) || len(value) > maxBytes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return false
		}
	}
	return true
}

func resolveExplicitNames(names []string, profiles map[string]int) ([]int, error) {
	if len(names) > MaxCandidateProfiles {
		return nil, ErrExpertLimit
	}
	result := make([]int, 0, min(len(names), MaxSelectedExperts))
	seen := make(map[string]struct{}, len(names))
	missing := 0
	for _, name := range names {
		if !boundedSingleLine(name, maxProfileNameBytes, false) || strings.ContainsAny(name, "/\\") {
			missing++
			continue
		}
		key := profileKey(name)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if len(seen) > MaxSelectedExperts {
			return nil, ErrExpertLimit
		}
		index, ok := profiles[key]
		if !ok {
			missing++
			continue
		}
		result = append(result, index)
	}
	if missing > 0 {
		return nil, fmt.Errorf("%w: %d requested profile(s) are unavailable", ErrUnknownExplicitProfile, missing)
	}
	return result, nil
}

func resolveLimit(configured, available, explicit int) (int, error) {
	if configured < 0 || configured > MaxSelectedExperts {
		return 0, ErrExpertLimit
	}
	limit := configured
	if limit == 0 {
		limit = DefaultMaxExperts
		if explicit > limit {
			limit = explicit
		}
	}
	if explicit > limit {
		return 0, ErrExpertLimit
	}
	return min(limit, available), nil
}

func selectTeam(profiles []candidate, explicit []int, limit int) []Selection {
	if len(explicit) > 0 {
		result := make([]Selection, 0, len(explicit))
		for _, index := range explicit {
			result = append(result, selection(profiles[index], 100, "Selected explicitly for the requested team."))
		}
		return result
	}
	ordered := append([]candidate(nil), profiles...)
	sortCandidatesByName(ordered)
	result := make([]Selection, 0, limit)
	for index := 0; index < limit; index++ {
		result = append(result, selection(ordered[index], 50, "Selected by stable team order."))
	}
	return result
}

func selectSwarm(ctx context.Context, profiles []candidate, explicit []int, limit int) ([]Selection, error) {
	selected := make([]candidate, 0, limit)
	selectedKeys := make(map[string]struct{}, limit)
	result := make([]Selection, 0, limit)
	for _, index := range explicit {
		current := profiles[index]
		selected = append(selected, current)
		selectedKeys[current.key] = struct{}{}
		result = append(result, selection(current, max(90, current.matchScore), "Selected explicitly as a required swarm member."))
	}
	for len(result) < limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bestIndex := -1
		bestScore := -1
		bestBase := -1
		for index := range profiles {
			if _, exists := selectedKeys[profiles[index].key]; exists || equivalentToAny(profiles[index], selected) {
				continue
			}
			similarity := maximumSimilarity(profiles[index].contractTokens, selected)
			diversity := 30 - similarity*30/100
			total := clampScore(profiles[index].matchScore*70/100 + diversity)
			if bestIndex < 0 || total > bestScore ||
				(total == bestScore && profiles[index].matchScore > bestBase) ||
				(total == bestScore && profiles[index].matchScore == bestBase && lessCandidate(profiles[index], profiles[bestIndex])) {
				bestIndex, bestScore, bestBase = index, total, profiles[index].matchScore
			}
		}
		if bestIndex < 0 {
			break
		}
		current := profiles[bestIndex]
		selected = append(selected, current)
		selectedKeys[current.key] = struct{}{}
		reason := "Selected for distinct profile coverage."
		if current.matchedSignals > 0 {
			reason = fmt.Sprintf("Selected for distinct coverage across %d bounded task signals.", current.matchedSignals)
		}
		result = append(result, selection(current, bestScore, reason))
	}
	return result, nil
}

func selectMoE(ctx context.Context, profiles []candidate, explicit []int, limit int) ([]Selection, error) {
	matched := make([]candidate, 0, len(profiles))
	for _, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if profile.matchScore > 0 {
			matched = append(matched, profile)
		}
	}
	if len(matched) == 0 {
		if len(explicit) == 0 {
			return nil, ErrNoMatch
		}
		result := make([]Selection, 0, len(explicit))
		for _, index := range explicit {
			result = append(result, selection(profiles[index], 0, "Selected as the explicit fallback because no profile matched."))
		}
		return result, nil
	}
	sort.Slice(matched, func(left, right int) bool {
		if matched[left].matchScore != matched[right].matchScore {
			return matched[left].matchScore > matched[right].matchScore
		}
		return lessCandidate(matched[left], matched[right])
	})
	limit = min(limit, len(matched))
	result := make([]Selection, 0, limit)
	for index := 0; index < limit; index++ {
		current := matched[index]
		reason := fmt.Sprintf("Matched %d bounded task signals in the profile contract.", current.matchedSignals)
		result = append(result, selection(current, current.matchScore, reason))
	}
	return result, nil
}

func lexicalMatch(prompt tokenSet, profile candidate) (score, signals int) {
	useMatches := intersectionCount(prompt, profile.useTokens)
	descriptionMatches := intersectionCount(prompt, profile.descriptionTokens)
	signals = intersectionCount(prompt, profile.contractTokens)
	return clampScore(useMatches*20 + descriptionMatches*8), signals
}

func selection(value candidate, score int, reason string) Selection {
	return Selection{
		Profile: cloneProfile(value.profile),
		Score:   clampScore(score),
		Reason:  boundUTF8(reason, MaxReasonBytes),
	}
}

func cloneProfile(value Profile) Profile {
	value.UseCases = append([]string(nil), value.UseCases...)
	return value
}

func profileKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func sortCandidatesByName(values []candidate) {
	sort.Slice(values, func(left, right int) bool { return lessCandidate(values[left], values[right]) })
}

func lessCandidate(left, right candidate) bool {
	if left.key != right.key {
		return left.key < right.key
	}
	if left.profile.Name != right.profile.Name {
		return left.profile.Name < right.profile.Name
	}
	return left.index < right.index
}

func clampScore(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func boundUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	marker := "…"
	cut := limit - len(marker)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut]) + marker
}
