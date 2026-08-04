package expertselector

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func testProfiles() []Profile {
	return []Profile{
		{
			Name: "Vidtrace", Model: "qwen-vision",
			Description: "Inspects video frames and media timelines for visual defects.",
			UseCases:    []string{"video frame inspection", "mp4 incident diagnosis", "visual media evidence"},
		},
		{
			Name: "Security", Model: "qwen-code",
			Description: "Assesses authentication, authorization, credentials, and vulnerabilities.",
			UseCases:    []string{"authentication security", "authorization review", "vulnerability assessment"},
		},
		{
			Name: "UX", Model: "qwen-ui",
			Description: "Reviews TUI and interface usability, keyboard behavior, and accessibility.",
			UseCases:    []string{"TUI keyboard accessibility", "UX interface design", "accessible navigation"},
		},
		{
			Name: "Backend", Model: "qwen-code",
			Description: "Designs APIs, database interactions, and backend services.",
			UseCases:    []string{"API implementation", "Postgres database design"},
		},
	}
}

func selectedNames(values []Selection) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = values[index].Profile.Name
	}
	return result
}

func assertBoundedSelections(t *testing.T, values []Selection, forbidden ...string) {
	t.Helper()
	for _, value := range values {
		if value.Score < 0 || value.Score > 100 {
			t.Fatalf("score is not bounded: %#v", value)
		}
		if len(value.Reason) > MaxReasonBytes || !utf8.ValidString(value.Reason) {
			t.Fatalf("reason is not bounded UTF-8: %q", value.Reason)
		}
		lower := strings.ToLower(value.Reason)
		for _, secret := range forbidden {
			if strings.Contains(lower, strings.ToLower(secret)) {
				t.Fatalf("reason leaked %q: %q", secret, value.Reason)
			}
		}
	}
}

func TestMoERoutesMediaPathToVidtraceWithoutLeakingIt(t *testing.T) {
	prompt := "inspect /Users/alice/private-incidents/customer-42-bug.mp4 and diagnose the visible failure"
	selected, err := Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: prompt, Profiles: testProfiles(),
		Options: Options{MaxExperts: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedNames(selected); !reflect.DeepEqual(got, []string{"Vidtrace"}) {
		t.Fatalf("selected = %v", got)
	}
	assertBoundedSelections(t, selected, "/users/", "alice", "customer-42", "bug.mp4", "private-incidents")
}

func TestMoESelectsSecurityAndUXForMultiProfileTask(t *testing.T) {
	request := Request{
		Strategy: StrategyMoE,
		Prompt:   "audit authentication security and keyboard accessibility in the TUI for /private/customer-42",
		Profiles: testProfiles(),
		Options:  Options{MaxExperts: 2},
	}
	first, err := Select(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Select(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("selection is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	names := selectedNames(first)
	if len(names) != 2 || !slices.Contains(names, "Security") || !slices.Contains(names, "UX") {
		t.Fatalf("selected = %v", names)
	}
	assertBoundedSelections(t, first, "/private/", "customer-42")
}

func TestTeamHonorsExplicitOrderCaseInsensitivelyAndDeduplicates(t *testing.T) {
	profiles := testProfiles()
	selected, err := Select(context.Background(), Request{
		Strategy: StrategyTeam, Profiles: profiles,
		Options: Options{ExplicitNames: []string{"ux", "SECURITY", "Ux"}, MaxExperts: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedNames(selected); !reflect.DeepEqual(got, []string{"UX", "Security"}) {
		t.Fatalf("selected = %v", got)
	}
	for _, value := range selected {
		if value.Score != 100 || !strings.Contains(value.Reason, "explicitly") {
			t.Fatalf("explicit team result = %#v", value)
		}
	}

	// Selection returns a deep copy and never mutates the caller's profiles.
	selected[0].Profile.UseCases[0] = "caller mutation"
	if profiles[2].UseCases[0] == "caller mutation" {
		t.Fatal("selection aliases caller-owned profile slices")
	}
}

func TestTeamMissingExplicitProfileFailsWithoutEchoingName(t *testing.T) {
	missing := "/private/customer/missing-expert"
	_, err := Select(context.Background(), Request{
		Strategy: StrategyTeam, Profiles: testProfiles(),
		Options: Options{ExplicitNames: []string{"Security", missing}},
	})
	if !errors.Is(err, ErrUnknownExplicitProfile) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), "/private/") {
		t.Fatalf("error leaked explicit input: %q", err)
	}

	_, err = Select(context.Background(), Request{
		Strategy: StrategyTeam, Profiles: testProfiles(),
		Options: Options{ExplicitNames: []string{"Security", "UX", "Backend"}, MaxExperts: 2},
	})
	if !errors.Is(err, ErrExpertLimit) {
		t.Fatalf("limit error = %v", err)
	}
}

func TestTeamDefaultUsesStableNameOrder(t *testing.T) {
	profiles := testProfiles()
	selected, err := Select(context.Background(), Request{
		Strategy: StrategyTeam, Profiles: []Profile{profiles[2], profiles[0], profiles[3], profiles[1]},
		Options: Options{MaxExperts: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedNames(selected); !reflect.DeepEqual(got, []string{"Backend", "Security", "UX"}) {
		t.Fatalf("selected = %v", got)
	}
}

func TestProfilesAndExplicitNamesDedupeCaseInsensitively(t *testing.T) {
	profiles := []Profile{
		{Name: "Vidtrace", Model: "first", Description: "video media", UseCases: []string{"video inspection"}},
		{Name: "vIDTRACE", Model: "second", Description: "security", UseCases: []string{"authentication"}},
	}
	duplicates := make([]string, 32)
	for index := range duplicates {
		duplicates[index] = "VIDTRACE"
	}
	selected, err := Select(context.Background(), Request{
		Strategy: StrategyTeam, Profiles: profiles,
		Options: Options{ExplicitNames: duplicates},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Profile.Model != "first" {
		t.Fatalf("case-insensitive dedupe = %#v", selected)
	}
}

func TestSwarmSelectsStableDiversityAndSkipsEquivalentProfiles(t *testing.T) {
	profiles := testProfiles()
	securityClone := profiles[1]
	securityClone.Name = "Security Clone"
	values := append(append([]Profile(nil), profiles...), securityClone)
	request := Request{
		Strategy: StrategySwarm,
		Prompt:   "review authentication security and accessible TUI keyboard behavior",
		Profiles: values,
		Options:  Options{MaxExperts: 3},
	}
	first, err := Select(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
	request.Profiles = values
	second, err := Select(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := selectedNames(first), selectedNames(second); !reflect.DeepEqual(got, want) {
		t.Fatalf("swarm tie break depends on input order: %v vs %v", got, want)
	}
	names := selectedNames(first)
	if len(names) != 3 || !slices.Contains(names, "Security") || !slices.Contains(names, "UX") || slices.Contains(names, "Security Clone") {
		t.Fatalf("swarm diversity = %v", names)
	}
	assertBoundedSelections(t, first)
}

func TestSwarmDoesNotFillLimitWithEquivalentProfiles(t *testing.T) {
	profiles := []Profile{
		{Name: "Alpha", Description: "authentication security", UseCases: []string{"security review"}},
		{Name: "Beta", Description: "authentication security", UseCases: []string{"security review"}},
		{Name: "Gamma", Description: "authentication security", UseCases: []string{"security review"}},
	}
	selected, err := Select(context.Background(), Request{
		Strategy: StrategySwarm, Prompt: "authentication security", Profiles: profiles,
		Options: Options{MaxExperts: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedNames(selected); !reflect.DeepEqual(got, []string{"Alpha"}) {
		t.Fatalf("equivalent swarm = %v", got)
	}
}

func TestMoEUsesExplicitFallbackOnlyWhenNothingMatches(t *testing.T) {
	selected, err := Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: "calculate an astronomy ephemeris", Profiles: testProfiles(),
		Options: Options{ExplicitNames: []string{"Backend", "UX"}, MaxExperts: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedNames(selected); !reflect.DeepEqual(got, []string{"Backend", "UX"}) {
		t.Fatalf("fallback = %v", got)
	}
	for _, value := range selected {
		if value.Score != 0 || !strings.Contains(value.Reason, "explicit fallback") {
			t.Fatalf("fallback result = %#v", value)
		}
	}

	_, err = Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: "calculate an astronomy ephemeris", Profiles: testProfiles(),
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("no-fallback error = %v", err)
	}
}

func TestMoEDeterministicTieBreakUsesProfileName(t *testing.T) {
	profiles := []Profile{
		{Name: "Beta", Description: "database", UseCases: []string{"Postgres database"}},
		{Name: "Alpha", Description: "database", UseCases: []string{"Postgres database"}},
	}
	selected, err := Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: "Postgres database", Profiles: profiles,
		Options: Options{MaxExperts: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedNames(selected); !reflect.DeepEqual(got, []string{"Alpha", "Beta"}) {
		t.Fatalf("tie order = %v", got)
	}
}

func TestMoEUsesOnlyContractTextAndWeightsUseCases(t *testing.T) {
	profiles := []Profile{
		{
			Name: "UseCase Match", Model: "unrelated-model",
			Description: "general bounded review", UseCases: []string{"authentication security"},
		},
		{
			Name: "Description Match", Model: "unrelated-model",
			Description: "authentication security", UseCases: []string{"general review"},
		},
		{
			Name: "Model Only", Model: "authentication-security",
			Description: "video media", UseCases: []string{"video inspection"},
		},
	}
	selected, err := Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: "authentication security", Profiles: profiles,
		Options: Options{MaxExperts: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedNames(selected); !reflect.DeepEqual(got, []string{"UseCase Match", "Description Match"}) {
		t.Fatalf("contract-only weighted selection = %v", got)
	}
	if selected[0].Score <= selected[1].Score {
		t.Fatalf("use_cases did not receive stronger weight: %#v", selected)
	}
}

func TestEmptyInvalidAndHugeInputsAreBounded(t *testing.T) {
	_, err := Select(context.Background(), Request{Strategy: StrategyTeam})
	if !errors.Is(err, ErrNoProfiles) {
		t.Fatalf("empty profiles error = %v", err)
	}
	_, err = Select(context.Background(), Request{Strategy: "other", Profiles: testProfiles()})
	if !errors.Is(err, ErrInvalidStrategy) {
		t.Fatalf("strategy error = %v", err)
	}
	//nolint:staticcheck // The public boundary must reject a nil context explicitly.
	_, err = Select(nil, Request{Strategy: StrategyTeam, Profiles: testProfiles()})
	if !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil context error = %v", err)
	}
	_, err = Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: string([]byte{0xff}), Profiles: testProfiles(),
	})
	if !errors.Is(err, ErrInvalidPrompt) {
		t.Fatalf("invalid prompt error = %v", err)
	}
	_, err = Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: strings.Repeat("x", MaxPromptBytes+1), Profiles: testProfiles(),
	})
	if !errors.Is(err, ErrPromptLimit) {
		t.Fatalf("prompt limit error = %v", err)
	}

	hugePrompt := strings.Repeat("bounded noise ", 100_000) + " /private/customer/sensitive-incident.mp4"
	selected, err := Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: hugePrompt, Profiles: testProfiles(),
		Options: Options{MaxExperts: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedNames(selected); !reflect.DeepEqual(got, []string{"Vidtrace"}) {
		t.Fatalf("huge prompt route = %v", got)
	}
	assertBoundedSelections(t, selected, "/private/", "customer", "sensitive-incident", "mp4")

	unicodePrompt := strings.Repeat("á", maxPromptLexicalBytes) + " /privado/niño-incidente.mp4"
	selected, err = Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: unicodePrompt, Profiles: testProfiles(),
		Options: Options{MaxExperts: 1},
	})
	if err != nil || len(selected) != 1 || selected[0].Profile.Name != "Vidtrace" {
		t.Fatalf("bounded Unicode route = %#v, %v", selected, err)
	}
	assertBoundedSelections(t, selected, "privado", "niño", "incidente")

	hugeProfile := Profile{Name: "Huge", Description: strings.Repeat("x", maxDescriptionBytes+1)}
	_, err = Select(context.Background(), Request{Strategy: StrategyTeam, Profiles: []Profile{hugeProfile}})
	if !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("huge profile error = %v", err)
	}
	tooMany := make([]Profile, MaxCandidateProfiles+1)
	for index := range tooMany {
		tooMany[index] = Profile{Name: "candidate-" + strings.Repeat("x", index%10)}
	}
	_, err = Select(context.Background(), Request{Strategy: StrategyTeam, Profiles: tooMany})
	if !errors.Is(err, ErrProfileLimit) {
		t.Fatalf("candidate limit error = %v", err)
	}
	_, err = Select(context.Background(), Request{
		Strategy: StrategyTeam, Profiles: testProfiles(), Options: Options{MaxExperts: MaxSelectedExperts + 1},
	})
	if !errors.Is(err, ErrExpertLimit) {
		t.Fatalf("expert limit error = %v", err)
	}
}

func TestEmptyMoEPromptRequiresAndHonorsExplicitFallback(t *testing.T) {
	_, err := Select(context.Background(), Request{Strategy: StrategyMoE, Profiles: testProfiles()})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("empty prompt error = %v", err)
	}
	selected, err := Select(context.Background(), Request{
		Strategy: StrategyMoE, Profiles: testProfiles(),
		Options: Options{ExplicitNames: []string{"Security"}, MaxExperts: 1},
	})
	if err != nil || len(selected) != 1 || selected[0].Profile.Name != "Security" {
		t.Fatalf("empty prompt fallback = %#v, %v", selected, err)
	}
}

func TestSelectHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	selected, err := Select(ctx, Request{Strategy: StrategySwarm, Prompt: strings.Repeat("video ", 10_000), Profiles: testProfiles()})
	if !errors.Is(err, context.Canceled) || selected != nil {
		t.Fatalf("canceled selection = %#v, %v", selected, err)
	}
}

func TestInvalidProfileAndUnknownExplicitValuesDoNotLeakPaths(t *testing.T) {
	_, err := Select(context.Background(), Request{
		Strategy: StrategyTeam,
		Profiles: []Profile{{Name: "/private/customer/expert", Description: "video"}},
	})
	if !errors.Is(err, ErrInvalidProfile) || strings.Contains(err.Error(), "/private/") {
		t.Fatalf("invalid profile error = %v", err)
	}

	_, err = Select(context.Background(), Request{
		Strategy: StrategyMoE, Prompt: "video", Profiles: testProfiles(),
		Options: Options{ExplicitNames: []string{"unknown-customer-secret"}},
	})
	if !errors.Is(err, ErrUnknownExplicitProfile) || strings.Contains(err.Error(), "customer-secret") {
		t.Fatalf("unknown explicit error = %v", err)
	}
}
