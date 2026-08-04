package ecosystem

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type bobMCPReceipt struct {
	SchemaVersion         int             `json:"schema_version"`
	OK                    bool            `json:"ok"`
	Workspace             string          `json:"workspace"`
	Clean                 *bool           `json:"clean"`
	LockChanged           *bool           `json:"lock_changed"`
	ConflictCount         *int            `json:"conflict_count"`
	PlanDigest            string          `json:"plan_digest"`
	PlanDigestQualified   string          `json:"plan_digest_qualified"`
	Authority             json.RawMessage `json:"authority"`
	Report                json.RawMessage `json:"report"`
	Counts                json.RawMessage `json:"counts"`
	Truncation            json.RawMessage `json:"truncation"`
	Recipe                json.RawMessage `json:"recipe"`
	Manifest              json.RawMessage `json:"manifest"`
	Stats                 json.RawMessage `json:"stats"`
	Source                string          `json:"source"`
	ManifestSchemaVersion *int            `json:"manifest_schema_version"`
	Enabled               *bool           `json:"enabled"`
	LocalOnly             *bool           `json:"local_only"`
	Warnings              json.RawMessage `json:"warnings"`
	NextActions           json.RawMessage `json:"next_actions"`
	Actions               []struct {
		Path string `json:"path"`
		Code string `json:"code"`
		Kind string `json:"kind"`
	} `json:"actions"`
	Context   json.RawMessage `json:"context"`
	Path      json.RawMessage `json:"path"`
	Operation string          `json:"operation"`
	List      json.RawMessage `json:"list"`
	Show      json.RawMessage `json:"show"`
	Plan      json.RawMessage `json:"plan"`
	Error     *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func projectBobReceipt(operation string, receipt RawReceipt) (DomainState, *ReceiptDigest, bool) {
	switch operation {
	case "bob_check", "bob_context", "bob_inspect", "bob_path", "bob_plan", "bob_playbook", "bob_recipe_describe", "bob_stats", "bob_validate_manifest":
	default:
		return "", nil, false
	}
	if isBobGuidanceOperation(operation) && !receiptDocumentWithinLimit(receipt, maxBobGuidanceDocumentBytes) {
		return "", nil, false
	}
	document, ok := receiptDocument(receipt)
	if !ok {
		return "", nil, false
	}
	var output bobMCPReceipt
	if json.Unmarshal(document, &output) != nil || output.SchemaVersion != 1 ||
		jsonObjectHasKey(document, "command") || jsonObjectHasKey(document, "data") {
		return "", nil, false
	}
	if isBobGuidanceOperation(operation) {
		return projectBobGuidanceReceipt(operation, document, output)
	}
	if output.Error != nil {
		if output.OK || strings.TrimSpace(output.Error.Code) == "" || strings.TrimSpace(output.Error.Code) != output.Error.Code ||
			strings.TrimSpace(output.Error.Message) == "" {
			return "", nil, false
		}
		return classifyBobErrorCode(output.Error.Code), nil, true
	}
	if !output.OK || !validBobMCPSuccess(operation, document, output) {
		return "", nil, false
	}
	if operation == "bob_inspect" {
		inspection, ok := validBobInspectReport(output.Report, output.Workspace)
		if !ok {
			return "", nil, false
		}
		return classifyBobInspection(inspection), nil, true
	}
	if operation != "bob_plan" && operation != "bob_check" {
		return DomainSucceeded, nil, true
	}
	if output.ConflictCount != nil && *output.ConflictCount > 0 {
		return DomainConflict, nil, true
	}
	for _, action := range output.Actions {
		if IsBobConflictCode(action.Code) || action.Kind == "conflict" {
			return DomainConflict, nil, true
		}
	}
	if (output.LockChanged != nil && *output.LockChanged) || (output.Clean != nil && !*output.Clean) {
		return DomainDrift, nil, true
	}
	return DomainSucceeded, nil, true
}

func validBobMCPSuccess(operation string, document json.RawMessage, output bobMCPReceipt) bool {
	switch operation {
	case "bob_inspect":
		_, reportOK := validBobInspectReport(output.Report, output.Workspace)
		return validBobWorkspace(output.Workspace) && validBobAuthority(output.Authority, output.Workspace, true) && reportOK
	case "bob_plan":
		return validBobWorkspace(output.Workspace) && validBobAuthority(output.Authority, output.Workspace, true) &&
			validBobPlanFields(document, output, true)
	case "bob_check":
		return validBobWorkspace(output.Workspace) && validBobAuthority(output.Authority, output.Workspace, true) &&
			validBobPlanFields(document, output, false)
	case "bob_validate_manifest":
		return validBobManifestSuccess(output)
	case "bob_recipe_describe":
		return validBobRecipeDescription(output.Recipe)
	case "bob_stats":
		return validBobAuthority(output.Authority, "", false) && output.Enabled != nil && output.LocalOnly != nil &&
			*output.LocalOnly && validBobStats(output.Stats, *output.Enabled)
	default:
		return false
	}
}

func validBobPlanFields(document json.RawMessage, output bobMCPReceipt, requireActions bool) bool {
	if output.Clean == nil || output.LockChanged == nil || output.ConflictCount == nil || *output.ConflictCount < 0 ||
		!validLowerHexDigest(output.PlanDigest) || !jsonKind(output.Warnings, '[') || !jsonKind(output.NextActions, '[') ||
		!validBobQualifiedPlanDigest(output.PlanDigestQualified, output.PlanDigest) {
		return false
	}
	counts, ok := validBobActionCounts(output.Counts)
	total := counts.create + counts.update + counts.adopt + counts.unchanged + counts.conflict
	if !ok || total == 0 || counts.conflict != *output.ConflictCount {
		return false
	}
	clean := !*output.LockChanged && counts.create == 0 && counts.update == 0 && counts.adopt == 0 && counts.conflict == 0
	if *output.Clean != clean {
		return false
	}
	if !requireActions {
		return !jsonObjectHasKey(document, "actions") && !jsonObjectHasKey(document, "truncation")
	}
	truncation, ok := validBobTruncation(output.Truncation)
	if !jsonObjectHasKey(document, "actions") || !ok || truncation.returned != len(output.Actions) ||
		truncation.total != total ||
		truncation.eligible+truncation.filtered != truncation.total ||
		(truncation.includeUnchanged && truncation.filtered != 0) ||
		(!truncation.includeUnchanged && truncation.filtered != counts.unchanged) {
		return false
	}
	projected := bobActionCountValues{}
	previousPath := ""
	for index, action := range output.Actions {
		if !validBobActionPath(action.Path) || !validBobActionKindCode(action.Kind, action.Code) {
			return false
		}
		if index > 0 && action.Path <= previousPath {
			return false
		}
		previousPath = action.Path
		projected.increment(action.Kind)
	}
	if !truncation.includeUnchanged && projected.unchanged != 0 {
		return false
	}
	return projected.lessThanOrEqual(counts)
}

type bobInspectionValues struct {
	repositoryState string
	degraded        bool
}

func validBobInspectReport(raw json.RawMessage, workspace string) (bobInspectionValues, bool) {
	if !jsonKind(raw, '{') {
		return bobInspectionValues{}, false
	}
	var report struct {
		SchemaVersion int             `json:"schema_version"`
		Workspace     string          `json:"workspace"`
		Repository    json.RawMessage `json:"repository"`
		Integrations  json.RawMessage `json:"integrations"`
		Degraded      *bool           `json:"degraded"`
		Warnings      json.RawMessage `json:"warnings"`
		NextActions   json.RawMessage `json:"next_actions"`
	}
	if json.Unmarshal(raw, &report) != nil || report.SchemaVersion != 1 || report.Workspace != workspace ||
		report.Degraded == nil || !jsonKind(report.Integrations, '[') || !jsonKind(report.Warnings, '[') ||
		!jsonKind(report.NextActions, '[') {
		return bobInspectionValues{}, false
	}
	state, ok := validBobRepositoryReport(report.Repository, workspace)
	if !ok {
		return bobInspectionValues{}, false
	}
	degraded, ok := validBobInspectIntegrations(report.Integrations, workspace)
	if !ok || degraded != *report.Degraded {
		return bobInspectionValues{}, false
	}
	return bobInspectionValues{repositoryState: state, degraded: *report.Degraded}, true
}

func validBobCLIInspectReport(raw json.RawMessage) (bobInspectionValues, bool) {
	var report struct {
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(raw, &report) != nil || !validBobWorkspace(report.Workspace) {
		return bobInspectionValues{}, false
	}
	return validBobInspectReport(raw, report.Workspace)
}

func validBobRecipeRef(raw json.RawMessage) (string, bool) {
	if !jsonKind(raw, '{') {
		return "", false
	}
	var recipe struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if json.Unmarshal(raw, &recipe) != nil {
		return "", false
	}
	switch recipe.ID {
	case "files":
		return recipe.ID, recipe.Version == 1
	case "go-agent-tool":
		// v5 kept the v4 manifest body; the bump only added a generated
		// registry-seam regression test on Bob's side.
		return recipe.ID, recipe.Version == 3 || recipe.Version == 4 || recipe.Version == 5
	default:
		if _, stack := bobStackRecipeRuntimes[recipe.ID]; stack {
			minVersion := 1
			if bobStackRecipesV2Only[recipe.ID] {
				minVersion = 2
			}
			return recipe.ID, recipe.Version >= minVersion && recipe.Version <= 2
		}
		return recipe.ID, false
	}
}

// bobStackRecipeRuntimes mirrors Bob's closed per-recipe runtime contract
// (manifest.stackRecipeRuntimes). Stack hygiene recipes share one manifest
// shape; only the admitted language/kind pairs differ.
var bobStackRecipeRuntimes = map[string]struct {
	languages []string
	kinds     []string
}{
	"ts-app":        {[]string{"typescript"}, []string{"app", "monorepo"}},
	"js-app":        {[]string{"javascript"}, []string{"app", "monorepo"}},
	"vue-app":       {[]string{"typescript", "javascript"}, []string{"web-app"}},
	"python-app":    {[]string{"python"}, []string{"app"}},
	"ruby-app":      {[]string{"ruby"}, []string{"app", "gem"}},
	"lua-lib":       {[]string{"lua"}, []string{"lib", "plugin"}},
	"rust-cli":      {[]string{"rust"}, []string{"cli", "lib", "workspace"}},
	"swift-package": {[]string{"swift"}, []string{"package"}},
	"elixir-app":    {[]string{"elixir"}, []string{"app", "umbrella"}},
	"static-web":    {[]string{"html"}, []string{"site"}},
}

// bobStackRecipesV2Only lists the stacks introduced with contract version 2;
// the other stack recipes remain renderable at their published version 1.
var bobStackRecipesV2Only = map[string]bool{
	"swift-package": true,
	"elixir-app":    true,
}

func isBobStackRecipe(recipeID string) bool {
	_, ok := bobStackRecipeRuntimes[recipeID]
	return ok
}

func bobJavaScriptFamilyRecipe(recipeID string) bool {
	return recipeID == "ts-app" || recipeID == "js-app" || recipeID == "vue-app"
}

// validBobStackRuntime enforces the recipe's closed language/kind contract.
// package_manager is admitted only for the JavaScript-family recipes and only
// from Bob's closed manager set.
func validBobStackRuntime(recipeID string, raw json.RawMessage) bool {
	runtime, ok := bobStackRecipeRuntimes[recipeID]
	if !ok || !jsonObjectHasKey(raw, "language") || !jsonObjectHasKey(raw, "kind") {
		return false
	}
	var value struct {
		Language       string `json:"language"`
		Kind           string `json:"kind"`
		PackageManager string `json:"package_manager"`
	}
	if json.Unmarshal(raw, &value) != nil ||
		!oneOf(value.Language, runtime.languages...) || !oneOf(value.Kind, runtime.kinds...) {
		return false
	}
	if bobJavaScriptFamilyRecipe(recipeID) {
		return oneOf(value.PackageManager, "", "bun", "npm", "pnpm", "yarn")
	}
	return value.PackageManager == ""
}

// validBobStackDistribution admits only the github_actions toggle: stack
// hygiene never distributes through goreleaser/homebrew and has no docs site.
func validBobStackDistribution(raw json.RawMessage) bool {
	value, ok := decodeBobManifestDistribution(raw)
	return ok && !*value.GoReleaser && !*value.Homebrew && (value.Docs == "" || value.Docs == "none")
}

// validBobQualifiedPlanDigest accepts the additive qualified digest only when
// it is exactly the sha256-prefixed spelling of the raw plan digest, so the
// value Bob recommends copying into `bob apply --expect-plan-digest` can
// never disagree with the legacy field.
func validBobQualifiedPlanDigest(qualified, digest string) bool {
	if qualified == "" {
		return true
	}
	return qualified == "sha256:"+digest
}

type bobActionCountValues struct {
	create    int
	update    int
	adopt     int
	unchanged int
	conflict  int
}

func (c *bobActionCountValues) increment(kind string) {
	switch kind {
	case "create":
		c.create++
	case "update":
		c.update++
	case "adopt":
		c.adopt++
	case "unchanged":
		c.unchanged++
	case "conflict":
		c.conflict++
	}
}

func (c bobActionCountValues) lessThanOrEqual(other bobActionCountValues) bool {
	return c.create <= other.create && c.update <= other.update && c.adopt <= other.adopt &&
		c.unchanged <= other.unchanged && c.conflict <= other.conflict
}

func validBobWorkspace(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00') && filepath.IsAbs(value)
}

func validBobActionPath(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || filepath.IsAbs(value) {
		return false
	}
	clean := filepath.Clean(value)
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func validBobAuthority(raw json.RawMessage, selected string, requireSelected bool) bool {
	if !jsonKind(raw, '{') {
		return false
	}
	var authority struct {
		Mode                  string `json:"mode"`
		DefaultWorkspace      string `json:"default_workspace"`
		SelectedWorkspace     string `json:"selected_workspace"`
		AllowedWorkspaceCount *int   `json:"allowed_workspace_count"`
	}
	if json.Unmarshal(raw, &authority) != nil ||
		(authority.Mode != "exact_allowlist" && authority.Mode != "any_workspace") ||
		!validBobWorkspace(authority.DefaultWorkspace) || authority.AllowedWorkspaceCount == nil ||
		*authority.AllowedWorkspaceCount < 1 {
		return false
	}
	if requireSelected {
		return validBobWorkspace(selected) && authority.SelectedWorkspace == selected
	}
	return authority.SelectedWorkspace == "" || validBobWorkspace(authority.SelectedWorkspace)
}

func validBobActionCounts(raw json.RawMessage) (bobActionCountValues, bool) {
	if !jsonKind(raw, '{') {
		return bobActionCountValues{}, false
	}
	var counts struct {
		Create    *int `json:"create"`
		Update    *int `json:"update"`
		Adopt     *int `json:"adopt"`
		Unchanged *int `json:"unchanged"`
		Conflict  *int `json:"conflict"`
	}
	if json.Unmarshal(raw, &counts) != nil || counts.Create == nil || counts.Update == nil ||
		counts.Adopt == nil || counts.Unchanged == nil || counts.Conflict == nil ||
		*counts.Create < 0 || *counts.Update < 0 || *counts.Adopt < 0 ||
		*counts.Unchanged < 0 || *counts.Conflict < 0 {
		return bobActionCountValues{}, false
	}
	return bobActionCountValues{
		create: *counts.Create, update: *counts.Update, adopt: *counts.Adopt,
		unchanged: *counts.Unchanged, conflict: *counts.Conflict,
	}, true
}

type bobTruncationValues struct {
	includeUnchanged bool
	max              int
	total            int
	eligible         int
	filtered         int
	returned         int
	byteLimitApplied bool
}

func validBobTruncation(raw json.RawMessage) (bobTruncationValues, bool) {
	if !jsonKind(raw, '{') {
		return bobTruncationValues{}, false
	}
	var value struct {
		IncludeUnchanged  *bool `json:"include_unchanged"`
		MaxActions        *int  `json:"max_actions"`
		TotalActions      *int  `json:"total_actions"`
		EligibleActions   *int  `json:"eligible_actions"`
		FilteredUnchanged *int  `json:"filtered_unchanged"`
		ReturnedActions   *int  `json:"returned_actions"`
		OmittedActions    *int  `json:"omitted_actions"`
		Truncated         *bool `json:"truncated"`
		OutputByteLimit   *int  `json:"output_byte_limit"`
		ByteLimitApplied  *bool `json:"byte_limit_applied"`
	}
	if json.Unmarshal(raw, &value) != nil || value.IncludeUnchanged == nil || value.MaxActions == nil ||
		value.TotalActions == nil || value.EligibleActions == nil || value.FilteredUnchanged == nil ||
		value.ReturnedActions == nil || value.OmittedActions == nil || value.Truncated == nil ||
		value.OutputByteLimit == nil || value.ByteLimitApplied == nil {
		return bobTruncationValues{}, false
	}
	initialReturned := *value.EligibleActions
	if initialReturned > *value.MaxActions {
		initialReturned = *value.MaxActions
	}
	valid := *value.MaxActions >= 1 && *value.MaxActions <= 500 && *value.TotalActions >= 0 && *value.EligibleActions >= 0 &&
		*value.FilteredUnchanged >= 0 && *value.ReturnedActions >= 0 && *value.OmittedActions >= 0 &&
		*value.OutputByteLimit == 30<<10 && *value.ReturnedActions <= initialReturned &&
		*value.OmittedActions == *value.EligibleActions-*value.ReturnedActions &&
		*value.Truncated == (*value.OmittedActions > 0) &&
		*value.ByteLimitApplied == (*value.ReturnedActions < initialReturned)
	if !valid {
		return bobTruncationValues{}, false
	}
	return bobTruncationValues{
		includeUnchanged: *value.IncludeUnchanged,
		max:              *value.MaxActions,
		total:            *value.TotalActions, eligible: *value.EligibleActions,
		filtered: *value.FilteredUnchanged, returned: *value.ReturnedActions,
		byteLimitApplied: *value.ByteLimitApplied,
	}, true
}

func validBobActionKindCode(kind, code string) bool {
	valid := map[string]map[string]bool{
		"create":    {"missing": true},
		"update":    {"mode_drift": true, "content_update": true},
		"adopt":     {"identical_content": true},
		"unchanged": {"in_sync": true},
		"conflict": {
			"unmanaged_differs": true, "managed_hash_mismatch": true, "managed_missing": true,
			"unmanaged_mode_differs": true, "retired_owned": true, "symlink": true, "special_file": true,
		},
	}
	return valid[kind] != nil && valid[kind][code]
}

func validBobInspectIntegrations(raw json.RawMessage, workspace string) (bool, bool) {
	if !jsonKind(raw, '[') {
		return false, false
	}
	var integrations []json.RawMessage
	if json.Unmarshal(raw, &integrations) != nil || len(integrations) != 2 {
		return false, false
	}
	seen := make(map[string]bool, len(integrations))
	degraded := false
	for _, rawIntegration := range integrations {
		var integration struct {
			Name      string          `json:"name"`
			Selected  *bool           `json:"selected"`
			Available *bool           `json:"available"`
			Probe     json.RawMessage `json:"probe"`
			Index     json.RawMessage `json:"index"`
		}
		if json.Unmarshal(rawIntegration, &integration) != nil || !oneOf(integration.Name, "codemap", "vecgrep") ||
			seen[integration.Name] || integration.Selected == nil || integration.Available == nil ||
			!jsonKind(integration.Probe, '{') || !jsonKind(integration.Index, '{') {
			return false, false
		}
		seen[integration.Name] = true
		var probe struct {
			State string          `json:"state"`
			CWD   string          `json:"cwd"`
			Argv  json.RawMessage `json:"argv"`
		}
		var index struct {
			State string `json:"state"`
		}
		if json.Unmarshal(integration.Probe, &probe) != nil || probe.CWD != workspace || !jsonKind(probe.Argv, '[') ||
			!oneOf(probe.State, "not_selected", "not_requested", "unavailable", "complete", "timed_out", "failed", "invalid_output", "wrong_project") ||
			json.Unmarshal(integration.Index, &index) != nil ||
			!oneOf(index.State, "not_indexed", "fresh", "stale", "incompatible", "unknown") {
			return false, false
		}
		if *integration.Selected {
			if probe.State == "not_selected" || (!*integration.Available && probe.State != "unavailable") ||
				(*integration.Available && probe.State == "unavailable") {
				return false, false
			}
			if !*integration.Available || probe.State != "complete" || index.State != "fresh" {
				degraded = true
			}
		} else if probe.State != "not_selected" || index.State != "unknown" {
			return false, false
		}
	}
	return degraded, seen["codemap"] && seen["vecgrep"]
}

func validBobRepositoryReport(raw json.RawMessage, workspace string) (string, bool) {
	if !jsonKind(raw, '{') {
		return "", false
	}
	var repository struct {
		State         string          `json:"state"`
		ManifestPath  string          `json:"manifest_path"`
		Recipe        string          `json:"recipe"`
		Ready         *bool           `json:"ready"`
		Converged     *bool           `json:"converged"`
		LockChanged   *bool           `json:"lock_changed"`
		ManagedFiles  *int            `json:"managed_files"`
		ConflictCount *int            `json:"conflict_count"`
		Actions       json.RawMessage `json:"actions"`
		Error         string          `json:"error"`
	}
	if json.Unmarshal(raw, &repository) != nil || repository.ManifestPath != filepath.Join(workspace, "bob.yaml") ||
		repository.Ready == nil || repository.Converged == nil || repository.LockChanged == nil ||
		repository.ManagedFiles == nil || repository.ConflictCount == nil ||
		*repository.ManagedFiles < 0 || *repository.ConflictCount < 0 {
		return "", false
	}
	counts, ok := validBobActionCounts(repository.Actions)
	if !ok || counts.conflict != *repository.ConflictCount {
		return "", false
	}
	total := counts.create + counts.update + counts.adopt + counts.unchanged + counts.conflict
	planned := repository.State == "conflicted" || repository.State == "clean" || repository.State == "drifted"
	if planned && (!oneOf(repository.Recipe, "files", "go-agent-tool") || total == 0) {
		return "", false
	}
	if planned && (*repository.ManagedFiles != total || repository.Error != "") {
		return "", false
	}
	switch repository.State {
	case "missing_manifest", "invalid_manifest":
		if repository.Recipe != "" || *repository.Ready || *repository.Converged || *repository.LockChanged ||
			*repository.ManagedFiles != 0 || total != 0 || strings.TrimSpace(repository.Error) == "" {
			return "", false
		}
	case "plan_error":
		if !oneOf(repository.Recipe, "files", "go-agent-tool") || *repository.Ready || *repository.Converged ||
			*repository.LockChanged || *repository.ManagedFiles != 0 || total != 0 || strings.TrimSpace(repository.Error) == "" {
			return "", false
		}
	case "conflicted":
		if *repository.Ready || *repository.Converged || *repository.ConflictCount == 0 {
			return "", false
		}
	case "clean":
		if !*repository.Ready || !*repository.Converged || *repository.LockChanged || *repository.ConflictCount != 0 ||
			counts.create != 0 || counts.update != 0 || counts.adopt != 0 {
			return "", false
		}
	case "drifted":
		if !*repository.Ready || *repository.Converged || *repository.ConflictCount != 0 ||
			(!*repository.LockChanged && counts.create == 0 && counts.update == 0 && counts.adopt == 0) {
			return "", false
		}
	default:
		return "", false
	}
	return repository.State, true
}

func classifyBobInspection(inspection bobInspectionValues) DomainState {
	switch inspection.repositoryState {
	case "missing_manifest", "invalid_manifest":
		return DomainBlocked
	case "plan_error":
		return DomainFailed
	case "conflicted":
		return DomainConflict
	case "drifted":
		return DomainDrift
	case "clean":
		if inspection.degraded {
			return DomainAttention
		}
		return DomainSucceeded
	default:
		return DomainUnknown
	}
}

func validBobManifestSuccess(output bobMCPReceipt) bool {
	if (output.Source != "inline" && output.Source != "workspace") || output.ManifestSchemaVersion == nil ||
		*output.ManifestSchemaVersion != 1 || !jsonKind(output.Warnings, '[') {
		return false
	}
	recipeID, ok := validBobRecipeRef(output.Recipe)
	if !ok || !validBobManifest(output.Manifest, recipeID) {
		return false
	}
	if output.Source == "workspace" {
		return validBobWorkspace(output.Workspace) && validBobAuthority(output.Authority, output.Workspace, true)
	}
	return output.Workspace == "" && validBobAuthority(output.Authority, "", false)
}

func validBobManifest(raw json.RawMessage, expectedRecipe string) bool {
	if !jsonKind(raw, '{') {
		return false
	}
	var manifest struct {
		SchemaVersion int             `json:"schema_version"`
		Recipe        string          `json:"recipe"`
		Product       json.RawMessage `json:"product"`
		Runtime       json.RawMessage `json:"runtime"`
		Surfaces      json.RawMessage `json:"surfaces"`
		Integrations  json.RawMessage `json:"integrations"`
		Distribution  json.RawMessage `json:"distribution"`
		Vars          json.RawMessage `json:"vars"`
		Files         json.RawMessage `json:"files"`
	}
	if json.Unmarshal(raw, &manifest) != nil || manifest.SchemaVersion != 1 || manifest.Recipe != expectedRecipe ||
		(expectedRecipe != "files" && expectedRecipe != "go-agent-tool" && !isBobStackRecipe(expectedRecipe)) ||
		!validBobProduct(manifest.Product, expectedRecipe) || !jsonKind(manifest.Runtime, '{') ||
		!jsonKind(manifest.Surfaces, '{') || !jsonKind(manifest.Integrations, '{') ||
		!jsonKind(manifest.Distribution, '{') {
		return false
	}
	if expectedRecipe == "go-agent-tool" {
		return validBobGoRuntime(manifest.Runtime) && validBobGoSurfaces(manifest.Surfaces) &&
			validBobGoIntegrations(manifest.Integrations) && validBobGoDistribution(manifest.Distribution, manifest.Product) &&
			!jsonObjectHasKey(raw, "vars") && !jsonObjectHasKey(raw, "files")
	}
	if isBobStackRecipe(expectedRecipe) {
		return validBobStackRuntime(expectedRecipe, manifest.Runtime) && validBobZeroSurfaces(manifest.Surfaces) &&
			validBobZeroIntegrations(manifest.Integrations) && validBobStackDistribution(manifest.Distribution) &&
			!jsonObjectHasKey(raw, "vars") && !jsonObjectHasKey(raw, "files")
	}
	return validBobZeroRuntime(manifest.Runtime) && validBobZeroSurfaces(manifest.Surfaces) &&
		validBobZeroIntegrations(manifest.Integrations) && validBobZeroDistribution(manifest.Distribution) &&
		validBobManifestVars(manifest.Vars) && validBobManifestFiles(manifest.Files)
}

func validBobProduct(raw json.RawMessage, recipe string) bool {
	if !jsonKind(raw, '{') {
		return false
	}
	for _, key := range []string{"name", "module", "description", "visibility", "license"} {
		if !jsonObjectHasKey(raw, key) {
			return false
		}
	}
	var product struct {
		Name        string `json:"name"`
		Module      string `json:"module"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
		License     string `json:"license"`
	}
	if json.Unmarshal(raw, &product) != nil || !validBobProductName(product.Name) ||
		strings.TrimSpace(product.Description) == "" ||
		!validBobModule(product.Module, recipe == "files" || isBobStackRecipe(recipe)) {
		return false
	}
	if recipe == "go-agent-tool" {
		return (product.Visibility == "public" || product.Visibility == "private") && product.License == "MIT"
	}
	return (product.Visibility == "" || product.Visibility == "public" || product.Visibility == "private") &&
		(product.License == "" || (strings.TrimSpace(product.License) != "" && len(product.License) <= 64))
}

func validBobProductName(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func validBobModule(value string, optional bool) bool {
	if value == "" || (optional && strings.TrimSpace(value) == "") {
		return optional
	}
	if strings.ContainsAny(value, " \t\r\n") || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.Contains(value, "//") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') ||
			strings.ContainsRune("-._~/", char) {
			continue
		}
		return false
	}
	return true
}

func validBobGoRuntime(raw json.RawMessage) bool {
	var value struct{ Language, Kind string }
	return jsonObjectHasKey(raw, "language") && jsonObjectHasKey(raw, "kind") &&
		json.Unmarshal(raw, &value) == nil && value.Language == "go" && value.Kind == "cli"
}

func validBobZeroRuntime(raw json.RawMessage) bool {
	var value struct{ Language, Kind string }
	return jsonObjectHasKey(raw, "language") && jsonObjectHasKey(raw, "kind") &&
		json.Unmarshal(raw, &value) == nil && value.Language == "" && value.Kind == ""
}

func validBobSurfaces(raw json.RawMessage, wantCLI, wantJSON bool) bool {
	var value struct {
		CLI    *bool `json:"cli"`
		JSON   *bool `json:"json"`
		MCP    *bool `json:"mcp"`
		Studio *bool `json:"studio"`
	}
	return json.Unmarshal(raw, &value) == nil && value.CLI != nil && value.JSON != nil && value.MCP != nil &&
		value.Studio != nil && *value.CLI == wantCLI && *value.JSON == wantJSON && !*value.MCP && !*value.Studio
}

func validBobGoSurfaces(raw json.RawMessage) bool { return validBobSurfaces(raw, true, true) }

func validBobZeroSurfaces(raw json.RawMessage) bool { return validBobSurfaces(raw, false, false) }

type bobManifestIntegrations struct {
	CodeStructure        string `json:"code_structure"`
	SemanticSearch       string `json:"semantic_search"`
	TerminalVerification string `json:"terminal_verification"`
	BrowserVerification  string `json:"browser_verification"`
	Secrets              string `json:"secrets"`
	Artifacts            string `json:"artifacts"`
}

func decodeBobManifestIntegrations(raw json.RawMessage) (bobManifestIntegrations, bool) {
	for _, key := range []string{"code_structure", "semantic_search", "terminal_verification", "browser_verification", "secrets", "artifacts"} {
		if !jsonObjectHasKey(raw, key) {
			return bobManifestIntegrations{}, false
		}
	}
	var value bobManifestIntegrations
	if json.Unmarshal(raw, &value) != nil {
		return bobManifestIntegrations{}, false
	}
	return value, true
}

func validBobGoIntegrations(raw json.RawMessage) bool {
	value, ok := decodeBobManifestIntegrations(raw)
	return ok && oneOf(value.CodeStructure, "none", "codemap") && oneOf(value.SemanticSearch, "none", "vecgrep") &&
		oneOf(value.TerminalVerification, "none", "glyphrun") && oneOf(value.BrowserVerification, "none", "cairntrace") &&
		oneOf(value.Secrets, "none", "tinyvault") && oneOf(value.Artifacts, "none", "fcheap")
}

func validBobZeroIntegrations(raw json.RawMessage) bool {
	value, ok := decodeBobManifestIntegrations(raw)
	return ok && value == (bobManifestIntegrations{})
}

type bobManifestDistribution struct {
	GitHubActions *bool  `json:"github_actions"`
	GoReleaser    *bool  `json:"goreleaser"`
	Homebrew      *bool  `json:"homebrew"`
	Docs          string `json:"docs"`
}

func decodeBobManifestDistribution(raw json.RawMessage) (bobManifestDistribution, bool) {
	var value bobManifestDistribution
	if json.Unmarshal(raw, &value) != nil || value.GitHubActions == nil || value.GoReleaser == nil ||
		value.Homebrew == nil || !jsonObjectHasKey(raw, "docs") {
		return bobManifestDistribution{}, false
	}
	return value, true
}

func validBobGoDistribution(raw, productRaw json.RawMessage) bool {
	value, ok := decodeBobManifestDistribution(raw)
	var product struct {
		Visibility string `json:"visibility"`
	}
	return ok && json.Unmarshal(productRaw, &product) == nil && oneOf(value.Docs, "none", "markdown") &&
		(!*value.Homebrew || (*value.GoReleaser && product.Visibility == "public"))
}

func validBobZeroDistribution(raw json.RawMessage) bool {
	value, ok := decodeBobManifestDistribution(raw)
	return ok && !*value.GitHubActions && !*value.GoReleaser && !*value.Homebrew && value.Docs == ""
}

func validBobManifestVars(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	if !jsonKind(raw, '{') {
		return false
	}
	var values map[string]string
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
		return false
	}
	for key := range values {
		if !validBobVarKey(key) {
			return false
		}
	}
	return true
}

func validBobVarKey(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func validBobManifestFiles(raw json.RawMessage) bool {
	if !jsonKind(raw, '[') {
		return false
	}
	var files []json.RawMessage
	if json.Unmarshal(raw, &files) != nil || len(files) == 0 {
		return false
	}
	seen := make(map[string]bool, len(files))
	for _, rawFile := range files {
		var file struct {
			Path    string  `json:"path"`
			Mode    string  `json:"mode"`
			Content *string `json:"content"`
		}
		if json.Unmarshal(rawFile, &file) != nil || strings.TrimSpace(file.Path) == "" ||
			!utf8.ValidString(file.Path) || file.Content == nil || !validBobFileMode(file.Mode) {
			return false
		}
		canonical := filepath.ToSlash(filepath.Clean(file.Path))
		if seen[canonical] {
			return false
		}
		seen[canonical] = true
	}
	return true
}

func validBobFileMode(value string) bool {
	if value == "" {
		return true
	}
	if len(value) < 3 || len(value) > 4 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '7' {
			return false
		}
	}
	return len(value) == 3 || value[0] == '0'
}

func validBobRecipeDescription(raw json.RawMessage) bool {
	recipeID, ok := validBobRecipeRef(raw)
	if !ok {
		return false
	}
	var recipe struct {
		ManifestSchemaVersion *int            `json:"manifest_schema_version"`
		Description           string          `json:"description"`
		Surfaces              []string        `json:"surfaces"`
		SupportedChoices      json.RawMessage `json:"supported_choices"`
	}
	if json.Unmarshal(raw, &recipe) != nil || recipe.ManifestSchemaVersion == nil ||
		*recipe.ManifestSchemaVersion != 1 || strings.TrimSpace(recipe.Description) == "" ||
		strings.TrimSpace(recipe.Description) != recipe.Description || len(recipe.Surfaces) == 0 ||
		!jsonObjectHasKey(raw, "supported_choices") {
		return false
	}
	if len(recipe.Surfaces) != 2 || recipe.Surfaces[0] != "cli" || recipe.Surfaces[1] != "json" {
		return false
	}
	choices := bytes.TrimSpace(recipe.SupportedChoices)
	if recipeID == "files" {
		return bytes.Equal(choices, []byte("null"))
	}
	if !jsonKind(choices, '[') {
		return false
	}
	var entries []struct {
		Field  string   `json:"field"`
		Values []string `json:"values"`
	}
	if json.Unmarshal(choices, &entries) != nil || len(entries) == 0 {
		return false
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.Field == "" || canonicalIdentifier(entry.Field) != entry.Field || seen[entry.Field] || len(entry.Values) == 0 {
			return false
		}
		seen[entry.Field] = true
		for _, value := range entry.Values {
			if value == "" || canonicalIdentifier(value) != value {
				return false
			}
		}
	}
	return true
}

func validBobCLIRecipeDescription(raw json.RawMessage) bool {
	if _, ok := validBobRecipeRef(raw); !ok {
		return false
	}
	var recipe struct {
		Description string   `json:"description"`
		Surfaces    []string `json:"surfaces"`
	}
	if json.Unmarshal(raw, &recipe) != nil || strings.TrimSpace(recipe.Description) == "" ||
		strings.TrimSpace(recipe.Description) != recipe.Description || len(recipe.Surfaces) != 2 ||
		recipe.Surfaces[0] != "cli" || recipe.Surfaces[1] != "json" {
		return false
	}
	return true
}

type bobTelemetryActionCounts struct {
	create    int
	update    int
	adopt     int
	unchanged int
	conflict  int
}

func decodeBobTelemetryActionCounts(raw json.RawMessage) (bobTelemetryActionCounts, bool) {
	if !jsonKind(raw, '{') {
		return bobTelemetryActionCounts{}, false
	}
	var value struct {
		Create    *int `json:"create"`
		Update    *int `json:"update"`
		Adopt     *int `json:"adopt"`
		Unchanged *int `json:"unchanged"`
		Conflict  *int `json:"conflict"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return bobTelemetryActionCounts{}, false
	}
	read := func(number *int) (int, bool) {
		if number == nil {
			return 0, true
		}
		return *number, *number >= 0
	}
	create, createOK := read(value.Create)
	update, updateOK := read(value.Update)
	adopt, adoptOK := read(value.Adopt)
	unchanged, unchangedOK := read(value.Unchanged)
	conflict, conflictOK := read(value.Conflict)
	return bobTelemetryActionCounts{create: create, update: update, adopt: adopt, unchanged: unchanged, conflict: conflict},
		createOK && updateOK && adoptOK && unchangedOK && conflictOK
}

func (c *bobTelemetryActionCounts) add(other bobTelemetryActionCounts) {
	c.create += other.create
	c.update += other.update
	c.adopt += other.adopt
	c.unchanged += other.unchanged
	c.conflict += other.conflict
}

func validBobStats(raw json.RawMessage, enabled bool) bool {
	if !jsonKind(raw, '{') {
		return false
	}
	var stats struct {
		SchemaVersion  int             `json:"schema_version"`
		Since          string          `json:"since"`
		Until          string          `json:"until"`
		Events         *int            `json:"events"`
		Successes      *int            `json:"successes"`
		Failures       *int            `json:"failures"`
		ConflictEvents *int            `json:"conflict_events"`
		DriftEvents    *int            `json:"drift_events"`
		DurationMS     *int64          `json:"duration_ms"`
		Actions        json.RawMessage `json:"actions"`
		Skipped        *int            `json:"skipped"`
		ByOperation    json.RawMessage `json:"by_operation"`
	}
	if json.Unmarshal(raw, &stats) != nil || stats.SchemaVersion != 1 ||
		stats.Events == nil || stats.Successes == nil || stats.Failures == nil || stats.ConflictEvents == nil ||
		stats.DriftEvents == nil || stats.DurationMS == nil || stats.Skipped == nil {
		return false
	}
	until, untilErr := time.Parse(time.RFC3339Nano, stats.Until)
	actions, actionsOK := decodeBobTelemetryActionCounts(stats.Actions)
	if untilErr != nil || !actionsOK ||
		*stats.Events < 0 || *stats.Successes < 0 || *stats.Failures < 0 ||
		*stats.ConflictEvents < 0 || *stats.DriftEvents < 0 || *stats.DurationMS < 0 || *stats.Skipped < 0 ||
		*stats.Events != *stats.Successes+*stats.Failures || *stats.ConflictEvents+*stats.DriftEvents > *stats.Failures {
		return false
	}
	if stats.Since != "" {
		since, err := time.Parse(time.RFC3339Nano, stats.Since)
		if err != nil || until.Before(since) {
			return false
		}
	}
	byOperation := bytes.TrimSpace(stats.ByOperation)
	if !enabled {
		emptyGroups := bytes.Equal(byOperation, []byte("null"))
		if jsonKind(byOperation, '[') {
			var groups []json.RawMessage
			emptyGroups = json.Unmarshal(byOperation, &groups) == nil && len(groups) == 0
		}
		return emptyGroups && *stats.Events == 0 && *stats.Successes == 0 &&
			*stats.Failures == 0 && *stats.ConflictEvents == 0 && *stats.DriftEvents == 0 &&
			*stats.DurationMS == 0 && *stats.Skipped == 0 && actions == (bobTelemetryActionCounts{})
	}
	if !jsonKind(byOperation, '[') {
		return false
	}
	var operations []json.RawMessage
	if json.Unmarshal(byOperation, &operations) != nil {
		return false
	}
	seen := make(map[string]bool, len(operations))
	totalEvents, totalSuccesses, totalFailures := 0, 0, 0
	totalConflict, totalDrift := 0, 0
	var totalDuration int64
	var totalActions bobTelemetryActionCounts
	for _, rawOperation := range operations {
		var operation struct {
			Operation      string          `json:"operation"`
			Events         *int            `json:"events"`
			Successes      *int            `json:"successes"`
			Failures       *int            `json:"failures"`
			ConflictEvents *int            `json:"conflict_events"`
			DriftEvents    *int            `json:"drift_events"`
			DurationMS     *int64          `json:"duration_ms"`
			Actions        json.RawMessage `json:"actions"`
		}
		if json.Unmarshal(rawOperation, &operation) != nil || seen[operation.Operation] ||
			!oneOf(operation.Operation, "new", "init", "plan", "apply", "check", "doctor", "inspect", "validate_manifest", "recipe_describe") ||
			operation.Events == nil || operation.Successes == nil || operation.Failures == nil ||
			operation.ConflictEvents == nil || operation.DriftEvents == nil || operation.DurationMS == nil {
			return false
		}
		operationActions, ok := decodeBobTelemetryActionCounts(operation.Actions)
		if !ok || *operation.Events < 0 || *operation.Successes < 0 || *operation.Failures < 0 ||
			*operation.ConflictEvents < 0 || *operation.DriftEvents < 0 || *operation.DurationMS < 0 ||
			*operation.Events != *operation.Successes+*operation.Failures ||
			*operation.ConflictEvents+*operation.DriftEvents > *operation.Failures {
			return false
		}
		seen[operation.Operation] = true
		totalEvents += *operation.Events
		totalSuccesses += *operation.Successes
		totalFailures += *operation.Failures
		totalConflict += *operation.ConflictEvents
		totalDrift += *operation.DriftEvents
		totalDuration += *operation.DurationMS
		totalActions.add(operationActions)
	}
	return totalEvents == *stats.Events && totalSuccesses == *stats.Successes && totalFailures == *stats.Failures &&
		totalConflict == *stats.ConflictEvents && totalDrift == *stats.DriftEvents && totalDuration == *stats.DurationMS &&
		totalActions == actions
}
