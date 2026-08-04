package ecosystem

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const (
	bobTestDigest        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	bobTestAuthority     = `{"mode":"exact_allowlist","default_workspace":"/repo","selected_workspace":"/repo","allowed_workspace_count":1}`
	bobTestCounts        = `{"create":0,"update":0,"adopt":0,"unchanged":1,"conflict":0}`
	bobTestStats         = `{"schema_version":1,"since":"2026-07-07T12:00:00Z","until":"2026-07-14T12:00:00Z","events":0,"successes":0,"failures":0,"conflict_events":0,"drift_events":0,"duration_ms":0,"actions":{},"skipped":0,"by_operation":null}`
	bobTestIntegrations  = `[{"name":"codemap","selected":false,"available":false,"probe":{"state":"not_selected","cwd":"/repo","argv":[]},"index":{"state":"unknown"}},{"name":"vecgrep","selected":false,"available":false,"probe":{"state":"not_selected","cwd":"/repo","argv":[]},"index":{"state":"unknown"}}]`
	bobTestFilesManifest = `{"schema_version":1,"recipe":"files","product":{"name":"sample","module":"","description":"Sample files","visibility":"","license":""},"runtime":{"language":"","kind":""},"surfaces":{"cli":false,"json":false,"mcp":false,"studio":false},"integrations":{"code_structure":"","semantic_search":"","terminal_verification":"","browser_verification":"","secrets":"","artifacts":""},"distribution":{"github_actions":false,"goreleaser":false,"homebrew":false,"docs":""},"files":[{"path":"README.md","content":"hello"}]}`
)

func bobTestInspectReceipt(repository, integrations string, degraded bool) string {
	return `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority +
		`,"report":{"schema_version":1,"workspace":"/repo","repository":` + repository +
		`,"integrations":` + integrations + `,"degraded":` + fmt.Sprintf("%t", degraded) +
		`,"warnings":[],"next_actions":[]}}`
}

func TestProjectToolCallPreservesLazyMCPHubRouteWithoutArguments(t *testing.T) {
	projection := ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "cortex",
		"tool":   "cortex__cortex_investigate",
		"arguments": map[string]any{
			"query": "SECRET QUERY MUST NOT PERSIST",
		},
	})
	if projection.Specialist != "cortex" || projection.Role != RoleCoordination {
		t.Fatalf("specialist projection = %#v", projection)
	}
	if projection.Route.Gateway != "mcphub" || projection.Route.Server != "cortex" || projection.Route.Tool != "cortex_investigate" || !projection.Route.Lazy {
		t.Fatalf("route = %#v", projection.Route)
	}
	if strings.Contains(strings.ToLower(projection.Operation), "secret") {
		t.Fatalf("projection retained arbitrary arguments: %#v", projection)
	}
}

func TestProjectToolCallUnderstandsNamespacedDirectAndGatewayTools(t *testing.T) {
	tests := []struct {
		name       string
		specialist string
		gateway    string
		server     string
		operation  string
	}{
		{"bob__bob_check", "bob", "", "bob", "bob_check"},
		{"mcphub__cortex__cortex_start_task", "cortex", "mcphub", "cortex", "cortex_start_task"},
		{"vecgrep_search", "vecgrep", "", "vecgrep", "vecgrep_search"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectToolCall(test.name, nil)
			if got.Specialist != test.specialist || got.Route.Gateway != test.gateway || got.Route.Server != test.server || got.Operation != test.operation {
				t.Fatalf("projection = %#v", got)
			}
		})
	}
}

func TestProjectToolResultSeparatesBobDomainFromTransport(t *testing.T) {
	projection := ProjectToolCall("bob__bob_check", nil)
	result := `{
		"schema_version":1,
		"ok":false,
		"command":"check",
		"data":{"clean":false,"plan":{"schema_version":1,"recipe":{"id":"files","version":1},"actions":[{"path":"README.md","kind":"update","code":"content_update"}],"conflict_count":0,"lock_changed":true}},
		"warnings":[],
		"next_actions":["bob apply"]
	}`
	got := ProjectToolResult(projection, result, false)
	if got.Transport != TransportSucceeded || got.Domain != DomainDrift || !got.NeedsAttention() || got.Successful() {
		t.Fatalf("drift projection = %#v", got)
	}

	got = ProjectToolResult(projection, "not a stable envelope", false)
	if got.Transport != TransportSucceeded || got.Domain != DomainUnknown || !got.NeedsAttention() {
		t.Fatalf("unknown Bob projection = %#v", got)
	}
}

func TestProjectReceiptPrefersStructuredBobMCPContract(t *testing.T) {
	projection := ProjectToolCall("bob__bob_check", nil)
	got := ProjectReceipt(projection, RawReceipt{
		Text: "human summary that must not be parsed",
		Structured: json.RawMessage(`{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority +
			`,"plan_digest":"` + bobTestDigest + `","clean":false,"lock_changed":false,"conflict_count":2,` +
			`"counts":{"create":0,"update":0,"adopt":0,"unchanged":0,"conflict":2},"warnings":[],"next_actions":[]}`),
		ToolError: false,
	})
	if got.Transport != TransportSucceeded || got.Domain != DomainConflict || !got.NeedsAttention() {
		t.Fatalf("structured Bob projection = %#v", got)
	}
}

func TestProjectReceiptAcceptsExactBobMCPSuccessContracts(t *testing.T) {
	tests := map[string]string{
		"bob_inspect": `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority +
			`,"report":{"schema_version":1,"workspace":"/repo","repository":{"state":"clean","manifest_path":"/repo/bob.yaml","recipe":"files","ready":true,"converged":true,"lock_changed":false,"managed_files":1,"conflict_count":0,"actions":` + bobTestCounts + `},"integrations":` + bobTestIntegrations + `,"degraded":false,"warnings":[],"next_actions":[]}}`,
		"bob_plan": `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority +
			`,"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":0,"counts":` + bobTestCounts +
			`,"actions":[],"truncation":{"include_unchanged":false,"max_actions":100,"total_actions":1,"eligible_actions":0,"filtered_unchanged":1,"returned_actions":0,"omitted_actions":0,"truncated":false,"output_byte_limit":30720,"byte_limit_applied":false},"warnings":[],"next_actions":[]}`,
		"bob_check": `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority +
			`,"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":0,"counts":` + bobTestCounts + `,"warnings":[],"next_actions":[]}`,
		"bob_validate_manifest": `{"schema_version":1,"ok":true,"source":"workspace","workspace":"/repo","authority":` + bobTestAuthority +
			`,"manifest_schema_version":1,"recipe":{"id":"files","version":1},"manifest":` + bobTestFilesManifest + `,"warnings":[]}`,
		"bob_recipe_describe": `{"schema_version":1,"ok":true,"recipe":{"id":"files","version":1,"manifest_schema_version":1,"description":"Files recipe","surfaces":["cli","json"],"supported_choices":null}}`,
		"bob_stats":           `{"schema_version":1,"ok":true,"enabled":false,"local_only":true,"authority":` + bobTestAuthority + `,"stats":` + bobTestStats + `}`,
	}
	for operation, structured := range tests {
		t.Run(operation, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("bob__"+operation, nil), RawReceipt{Structured: json.RawMessage(structured)})
			if got.Transport != TransportSucceeded || got.Domain != DomainSucceeded || !got.DomainTyped || !got.Successful() {
				t.Fatalf("exact Bob MCP success = %#v", got)
			}
		})
	}
}

func TestProjectReceiptClassifiesExactBobInspectStatesAndDegradation(t *testing.T) {
	zeroCounts := `{"create":0,"update":0,"adopt":0,"unchanged":0,"conflict":0}`
	tests := []struct {
		name         string
		repository   string
		integrations string
		degraded     bool
		domain       DomainState
		typed        bool
	}{
		{
			name: "clean", integrations: bobTestIntegrations, domain: DomainSucceeded, typed: true,
			repository: `{"state":"clean","manifest_path":"/repo/bob.yaml","recipe":"files","ready":true,"converged":true,"lock_changed":false,"managed_files":1,"conflict_count":0,"actions":` + bobTestCounts + `}`,
		},
		{
			name: "drifted", integrations: bobTestIntegrations, domain: DomainDrift, typed: true,
			repository: `{"state":"drifted","manifest_path":"/repo/bob.yaml","recipe":"files","ready":true,"converged":false,"lock_changed":false,"managed_files":1,"conflict_count":0,"actions":{"create":0,"update":1,"adopt":0,"unchanged":0,"conflict":0}}`,
		},
		{
			name: "conflicted", integrations: bobTestIntegrations, domain: DomainConflict, typed: true,
			repository: `{"state":"conflicted","manifest_path":"/repo/bob.yaml","recipe":"files","ready":false,"converged":false,"lock_changed":false,"managed_files":1,"conflict_count":1,"actions":{"create":0,"update":0,"adopt":0,"unchanged":0,"conflict":1}}`,
		},
		{
			name: "missing manifest", integrations: bobTestIntegrations, domain: DomainBlocked, typed: true,
			repository: `{"state":"missing_manifest","manifest_path":"/repo/bob.yaml","ready":false,"converged":false,"lock_changed":false,"managed_files":0,"conflict_count":0,"actions":` + zeroCounts + `,"error":"bob.yaml is missing"}`,
		},
		{
			name: "selected integration unavailable", degraded: true, domain: DomainAttention, typed: true,
			integrations: `[{"name":"codemap","selected":true,"available":false,"probe":{"state":"unavailable","cwd":"/repo","argv":[]},"index":{"state":"unknown"}},{"name":"vecgrep","selected":false,"available":false,"probe":{"state":"not_selected","cwd":"/repo","argv":[]},"index":{"state":"unknown"}}]`,
			repository:   `{"state":"clean","manifest_path":"/repo/bob.yaml","recipe":"files","ready":true,"converged":true,"lock_changed":false,"managed_files":1,"conflict_count":0,"actions":` + bobTestCounts + `}`,
		},
		{
			name: "contradictory degraded flag", degraded: false, domain: DomainUnknown, typed: false,
			integrations: `[{"name":"codemap","selected":true,"available":false,"probe":{"state":"unavailable","cwd":"/repo","argv":[]},"index":{"state":"unknown"}},{"name":"vecgrep","selected":false,"available":false,"probe":{"state":"not_selected","cwd":"/repo","argv":[]},"index":{"state":"unknown"}}]`,
			repository:   `{"state":"clean","manifest_path":"/repo/bob.yaml","recipe":"files","ready":true,"converged":true,"lock_changed":false,"managed_files":1,"conflict_count":0,"actions":` + bobTestCounts + `}`,
		},
		{
			name: "zero-file clean report is impossible", integrations: bobTestIntegrations, domain: DomainUnknown,
			repository: `{"state":"clean","manifest_path":"/repo/bob.yaml","recipe":"files","ready":true,"converged":true,"lock_changed":false,"managed_files":0,"conflict_count":0,"actions":` + zeroCounts + `}`,
		},
		{
			name: "repository identity must match workspace", integrations: bobTestIntegrations, domain: DomainUnknown,
			repository: `{"state":"clean","manifest_path":"/other/bob.yaml","recipe":"evil","ready":true,"converged":true,"lock_changed":false,"managed_files":1,"conflict_count":0,"actions":` + bobTestCounts + `}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("bob__bob_inspect", nil), RawReceipt{Structured: json.RawMessage(
				bobTestInspectReceipt(test.repository, test.integrations, test.degraded),
			)})
			if got.Domain != test.domain || got.DomainTyped != test.typed || got.Successful() != (test.domain == DomainSucceeded) {
				t.Fatalf("inspect projection = %#v", got)
			}
		})
	}
}

func TestProjectReceiptRejectsBobPlanInvariantMismatches(t *testing.T) {
	tests := map[string]string{
		"returned kind exceeds counts": `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority + `,"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":0,"counts":` + bobTestCounts + `,"actions":[{"path":"a","kind":"create","code":"missing"}],"truncation":{"include_unchanged":true,"max_actions":100,"total_actions":1,"eligible_actions":1,"filtered_unchanged":0,"returned_actions":1,"omitted_actions":0,"truncated":false,"output_byte_limit":30720,"byte_limit_applied":false},"warnings":[],"next_actions":[]}`,
		"returned exceeds max":         `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority + `,"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":0,"counts":{"create":0,"update":0,"adopt":0,"unchanged":2,"conflict":0},"actions":[{"path":"a","kind":"unchanged","code":"in_sync"},{"path":"b","kind":"unchanged","code":"in_sync"}],"truncation":{"include_unchanged":true,"max_actions":1,"total_actions":2,"eligible_actions":2,"filtered_unchanged":0,"returned_actions":2,"omitted_actions":0,"truncated":false,"output_byte_limit":30720,"byte_limit_applied":false},"warnings":[],"next_actions":[]}`,
		"byte trimming flag mismatch":  `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority + `,"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":0,"counts":{"create":0,"update":0,"adopt":0,"unchanged":2,"conflict":0},"actions":[{"path":"a","kind":"unchanged","code":"in_sync"}],"truncation":{"include_unchanged":true,"max_actions":2,"total_actions":2,"eligible_actions":2,"filtered_unchanged":0,"returned_actions":1,"omitted_actions":1,"truncated":true,"output_byte_limit":30720,"byte_limit_applied":false},"warnings":[],"next_actions":[]}`,
		"duplicate action path":        `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority + `,"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":0,"counts":{"create":0,"update":0,"adopt":0,"unchanged":2,"conflict":0},"actions":[{"path":"a","kind":"unchanged","code":"in_sync"},{"path":"a","kind":"unchanged","code":"in_sync"}],"truncation":{"include_unchanged":true,"max_actions":2,"total_actions":2,"eligible_actions":2,"filtered_unchanged":0,"returned_actions":2,"omitted_actions":0,"truncated":false,"output_byte_limit":30720,"byte_limit_applied":false},"warnings":[],"next_actions":[]}`,
	}
	for name, structured := range tests {
		t.Run(name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("bob__bob_plan", nil), RawReceipt{Structured: json.RawMessage(structured)})
			if got.Domain != DomainUnknown || got.DomainTyped || got.Successful() {
				t.Fatalf("malformed plan projection = %#v", got)
			}
		})
	}
}

func TestProjectReceiptHandlesBobCLIProjectionAmbiguityAndExactActions(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		text      string
		domain    DomainState
		typed     bool
	}{
		{
			name: "empty conflicts-only plan is attention", operation: "bob_plan", domain: DomainAttention, typed: true,
			text: `{"schema_version":1,"ok":true,"command":"plan","data":{"schema_version":1,"recipe":{"id":"files","version":1},"lock_changed":false,"conflict_count":0,"actions":[]},"warnings":[],"next_actions":[]}`,
		},
		{
			name: "kind code mismatch stays unknown", operation: "bob_plan", domain: DomainUnknown,
			text: `{"schema_version":1,"ok":true,"command":"plan","data":{"schema_version":1,"recipe":{"id":"files","version":1},"lock_changed":false,"conflict_count":0,"actions":[{"path":"a","kind":"create","code":"in_sync"}]},"warnings":[],"next_actions":[]}`,
		},
		{
			name: "duplicate paths stay unknown", operation: "bob_plan", domain: DomainUnknown,
			text: `{"schema_version":1,"ok":true,"command":"plan","data":{"schema_version":1,"recipe":{"id":"files","version":1},"lock_changed":false,"conflict_count":0,"actions":[{"path":"a","kind":"unchanged","code":"in_sync"},{"path":"a","kind":"unchanged","code":"in_sync"}]},"warnings":[],"next_actions":[]}`,
		},
		{
			name: "retired whitespace path remains a conflict", operation: "bob_plan", domain: DomainConflict, typed: true,
			text: `{"schema_version":1,"ok":true,"command":"plan","data":{"schema_version":1,"recipe":{"id":"files","version":1},"lock_changed":false,"conflict_count":1,"actions":[{"path":" ","kind":"conflict","code":"retired_owned"}]},"warnings":[],"next_actions":[]}`,
		},
		{
			name: "clean check cannot carry create action", operation: "bob_check", domain: DomainUnknown,
			text: `{"schema_version":1,"ok":true,"command":"check","data":{"clean":true,"plan":{"schema_version":1,"recipe":{"id":"files","version":1},"lock_changed":false,"conflict_count":0,"actions":[{"path":"a","kind":"create","code":"missing"}]}},"warnings":[],"next_actions":[]}`,
		},
		{
			name: "disabled stats accepts whitespace workspace and empty groups", operation: "bob_stats", domain: DomainSucceeded, typed: true,
			text: `{"schema_version":1,"ok":true,"command":"stats","data":{"enabled":false,"local_only":true,"selection":"/repo ","stats":{"schema_version":1,"until":"2026-07-14T12:00:00Z","events":0,"successes":0,"failures":0,"conflict_events":0,"drift_events":0,"duration_ms":0,"actions":{},"skipped":0,"by_operation":[]}},"warnings":[],"next_actions":[]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("bob__"+test.operation, nil), RawReceipt{Text: test.text})
			if got.Domain != test.domain || got.DomainTyped != test.typed {
				t.Fatalf("CLI projection = %#v", got)
			}
		})
	}
}

func TestProjectReceiptValidatesBobRecipeManifestAndStatsExactness(t *testing.T) {
	goManifest := `{"schema_version":1,"recipe":"go-agent-tool","product":{"name":"sample","module":"example.com/sample","description":"Sample tool","visibility":"public","license":"MIT"},"runtime":{"language":"go","kind":"cli"},"surfaces":{"cli":true,"json":true,"mcp":false,"studio":false},"integrations":{"code_structure":"none","semantic_search":"none","terminal_verification":"none","browser_verification":"none","secrets":"none","artifacts":"none"},"distribution":{"github_actions":true,"goreleaser":true,"homebrew":true,"docs":"markdown"}}`
	validManifest := `{"schema_version":1,"ok":true,"source":"workspace","workspace":"/repo","authority":` + bobTestAuthority + `,"manifest_schema_version":1,"recipe":{"id":"go-agent-tool","version":3},"manifest":` + goManifest + `,"warnings":[]}`
	if got := ProjectReceipt(ProjectToolCall("bob__bob_validate_manifest", nil), RawReceipt{Structured: json.RawMessage(validManifest)}); !got.Successful() || !got.DomainTyped {
		t.Fatalf("valid go manifest projection = %#v", got)
	}
	privateHomebrew := strings.Replace(goManifest, `"visibility":"public"`, `"visibility":"private"`, 1)
	invalidManifest := strings.Replace(validManifest, goManifest, privateHomebrew, 1)
	if got := ProjectReceipt(ProjectToolCall("bob__bob_validate_manifest", nil), RawReceipt{Structured: json.RawMessage(invalidManifest)}); got.Domain != DomainUnknown || got.DomainTyped {
		t.Fatalf("private homebrew manifest projection = %#v", got)
	}
	badRecipe := `{"schema_version":1,"ok":true,"recipe":{"id":"files","version":999,"manifest_schema_version":1,"description":"Files recipe","surfaces":["cli","json"],"supported_choices":null}}`
	if got := ProjectReceipt(ProjectToolCall("bob__bob_recipe_describe", nil), RawReceipt{Structured: json.RawMessage(badRecipe)}); got.Domain != DomainUnknown || got.DomainTyped {
		t.Fatalf("bad recipe stamp projection = %#v", got)
	}

	validStats := `{"schema_version":1,"ok":true,"enabled":true,"local_only":true,"authority":` + bobTestAuthority + `,"stats":{"schema_version":1,"since":"2026-07-07T12:00:00Z","until":"2026-07-14T12:00:00Z","events":1,"successes":0,"failures":1,"conflict_events":1,"drift_events":0,"duration_ms":5,"actions":{"conflict":1},"skipped":0,"by_operation":[{"operation":"plan","events":1,"successes":0,"failures":1,"conflict_events":1,"drift_events":0,"duration_ms":5,"actions":{"conflict":1}}]}}`
	if got := ProjectReceipt(ProjectToolCall("bob__bob_stats", nil), RawReceipt{Structured: json.RawMessage(validStats)}); !got.Successful() || !got.DomainTyped {
		t.Fatalf("valid stats projection = %#v", got)
	}
	impossibleStats := strings.Replace(validStats, `"events":1,"successes":0,"failures":1`, `"events":1,"successes":1,"failures":1`, 1)
	if got := ProjectReceipt(ProjectToolCall("bob__bob_stats", nil), RawReceipt{Structured: json.RawMessage(impossibleStats)}); got.Domain != DomainUnknown || got.DomainTyped {
		t.Fatalf("impossible stats projection = %#v", got)
	}
}

func TestProjectReceiptRejectsMalformedOrCrossOperationBobSuccess(t *testing.T) {
	tests := []struct {
		name       string
		operation  string
		structured string
		text       string
	}{
		{name: "minimal ok true", operation: "bob_check", structured: `{"schema_version":1,"ok":true}`},
		{name: "foreign plan fields cannot type stats", operation: "bob_stats", structured: `{"schema_version":1,"ok":true,"clean":false,"conflict_count":2}`},
		{name: "empty authority", operation: "bob_check", structured: `{"schema_version":1,"ok":true,"workspace":"/repo","authority":{},"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":0,"counts":` + bobTestCounts + `,"warnings":[],"next_actions":[]}`},
		{name: "negative conflict count", operation: "bob_plan", structured: `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority + `,"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":-1,"counts":` + bobTestCounts + `,"actions":[],"truncation":{"include_unchanged":false,"max_actions":100,"total_actions":0,"eligible_actions":0,"filtered_unchanged":0,"returned_actions":0,"omitted_actions":0,"truncated":false,"output_byte_limit":30720,"byte_limit_applied":false},"warnings":[],"next_actions":[]}`},
		{name: "short plan digest", operation: "bob_check", structured: `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority + `,"plan_digest":"abc","clean":true,"lock_changed":false,"conflict_count":0,"counts":` + bobTestCounts + `,"warnings":[],"next_actions":[]}`},
		{name: "future schema", operation: "bob_check", structured: `{"schema_version":2,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority + `,"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":0,"counts":` + bobTestCounts + `,"warnings":[],"next_actions":[]}`},
		{name: "plan cannot type check", operation: "bob_check", structured: `{"schema_version":1,"ok":true,"workspace":"/repo","authority":` + bobTestAuthority + `,"plan_digest":"` + bobTestDigest + `","clean":true,"lock_changed":false,"conflict_count":0,"counts":` + bobTestCounts + `,"actions":[],"truncation":{"include_unchanged":false,"max_actions":100,"total_actions":0,"eligible_actions":0,"filtered_unchanged":0,"returned_actions":0,"omitted_actions":0,"truncated":false,"output_byte_limit":30720,"byte_limit_applied":false},"warnings":[],"next_actions":[]}`},
		{name: "manifest recipe ref cannot type describe", operation: "bob_recipe_describe", structured: `{"schema_version":1,"ok":true,"recipe":{"id":"files","version":1}}`},
		{name: "mismatched CLI command", operation: "bob_check", text: bobPlanCleanJSON},
		{name: "empty CLI data", operation: "bob_check", text: `{"schema_version":1,"ok":true,"command":"check","data":{},"warnings":[],"next_actions":[]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("bob__"+test.operation, nil), RawReceipt{
				Structured: json.RawMessage(test.structured), Text: test.text,
			})
			if got.Transport != TransportSucceeded || got.Domain != DomainUnknown || got.DomainTyped || got.Successful() {
				t.Fatalf("malformed Bob success = %#v", got)
			}
		})
	}
}

func TestProjectReceiptIgnoresForeignPlanFieldsOnValidBobStats(t *testing.T) {
	got := ProjectReceipt(ProjectToolCall("bob__bob_stats", nil), RawReceipt{Structured: json.RawMessage(
		`{"schema_version":1,"ok":true,"enabled":false,"local_only":true,"authority":` + bobTestAuthority +
			`,"stats":` + bobTestStats + `,"clean":false,"conflict_count":2}`,
	)})
	if got.Domain != DomainSucceeded || !got.DomainTyped || !got.Successful() {
		t.Fatalf("valid Bob stats with foreign fields = %#v", got)
	}
}

func TestProjectReceiptClassifiesBobMCPAndCLIErrorContractsConsistently(t *testing.T) {
	tests := []struct {
		name       string
		receipt    RawReceipt
		wantDomain DomainState
	}{
		{
			name: "flat MCP workspace authority rejection",
			receipt: RawReceipt{
				Structured: json.RawMessage(`{"schema_version":1,"ok":false,"error":{"code":"workspace_unauthorized","message":"outside startup authority"}}`),
				ToolError:  true,
			},
			wantDomain: DomainBlocked,
		},
		{
			name: "nested CLI workspace authority rejection",
			receipt: RawReceipt{
				Text:      `{"schema_version":1,"ok":false,"command":"check","data":{"error":{"code":"workspace_unauthorized","message":"outside startup authority"}},"warnings":[],"next_actions":[]}`,
				ToolError: true,
			},
			wantDomain: DomainBlocked,
		},
		{
			name: "unknown MCP error fails closed",
			receipt: RawReceipt{
				Structured: json.RawMessage(`{"schema_version":1,"ok":false,"error":{"code":"workspace_out_of_scope","message":"unsupported legacy code"}}`),
				ToolError:  true,
			},
			wantDomain: DomainFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("bob__bob_check", nil), test.receipt)
			if got.Transport != TransportSucceeded || got.Domain != test.wantDomain || !got.DomainTyped {
				t.Fatalf("Bob projection = %#v, want domain %s", got, test.wantDomain)
			}
		})
	}
}

func TestProjectReceiptAcceptsCurrentBobCLIShapesAndLegacyDrift(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		text      string
		domain    DomainState
	}{
		{
			name: "disabled stats with null operation groups", operation: "bob_stats",
			text:   `{"schema_version":1,"ok":true,"command":"stats","data":{"enabled":false,"local_only":true,"selection":"/repo","stats":` + bobTestStats + `},"warnings":[],"next_actions":[]}`,
			domain: DomainSucceeded,
		},
		{
			name: "recipe show success", operation: "bob_recipe_describe",
			text:   `{"schema_version":1,"ok":true,"command":"recipe show","data":{"id":"files","version":1,"description":"Files recipe","surfaces":["cli","json"]},"warnings":[],"next_actions":[]}`,
			domain: DomainSucceeded,
		},
		{
			name: "recipe show generic command failure", operation: "bob_recipe_describe",
			text:   `{"schema_version":1,"ok":false,"command":"recipe","data":{"error":{"code":"recipe_unknown","message":"unknown recipe"}},"warnings":[],"next_actions":[]}`,
			domain: DomainBlocked,
		},
		{
			name: "legacy action kind reports drift", operation: "bob_plan",
			text:   `{"schema_version":1,"ok":true,"command":"plan","data":{"schema_version":1,"recipe":{"id":"files","version":1},"lock_changed":false,"conflict_count":0,"actions":[{"path":"README.md","kind":"update"}]},"warnings":[],"next_actions":[]}`,
			domain: DomainDrift,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("bob__"+test.operation, nil), RawReceipt{Text: test.text})
			if got.Transport != TransportSucceeded || got.Domain != test.domain || !got.DomainTyped {
				t.Fatalf("Bob CLI projection = %#v, want %s", got, test.domain)
			}
		})
	}
}

func TestProjectReceiptDoesNotFallbackFromRejectedBobStructuredContent(t *testing.T) {
	got := ProjectReceipt(ProjectToolCall("bob__bob_check", nil), RawReceipt{
		Structured: json.RawMessage("null"),
		Text:       `{"schema_version":1,"ok":false,"command":"check","data":{"error":{"code":"workspace_unauthorized","message":"must stay behind the parser boundary"}},"warnings":[],"next_actions":[]}`,
	})
	if got.Transport != TransportSucceeded || got.Domain != DomainUnknown || got.DomainTyped {
		t.Fatalf("rejected structured Bob receipt = %#v, want untyped unknown", got)
	}
}

func TestProjectReceiptParsesVersionedVerifierOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		structured string
		toolError  bool
		domain     DomainState
		evidence   EvidenceState
	}{
		{
			name: "glyphrun__glyph_run", toolError: true,
			structured: `{"schemaVersion":1,"runId":"run-1","specName":"smoke","status":"failed","startedAt":"2026-07-13T00:00:00Z","endedAt":"2026-07-13T00:00:01Z","durationMs":1000,"target":{"cmd":["task","dev"]},"terminal":{"cols":80,"rows":24,"profile":"xterm-256color"},"outcomes":[{"id":"launch","status":"failed"}],"artifacts":{},"runDir":"/tmp/run-1","exitCode":1}`,
			domain:     DomainFailed, evidence: EvidenceContradicted,
		},
		{
			name:       "cairntrace__cairn_run",
			structured: `{"$schema":"urn:cairntrace.dev:run:v1","version":"1","runId":"run-1","runDir":"/tmp/run-1","spec":{"name":"smoke","path":"/tmp/smoke.yml"},"environment":"test","backend":"playwright","coldStart":true,"status":"passed","startedAt":"2026-07-13T00:00:00Z","endedAt":"2026-07-13T00:00:01Z","durationMs":1000,"outcomes":[{"id":"launch","status":"passed"}],"steps":[],"artifacts":{"agentContext":"agent-context.md","events":"events.ndjson"},"exitCode":0}`,
			domain:     DomainSucceeded, evidence: EvidenceVerified,
		},
		{
			name:       "cairntrace__cairn_run",
			structured: `{"$schema":"urn:cairntrace.dev:run:v2","version":"2","status":"passed"}`,
			domain:     DomainUnknown, evidence: EvidenceNone,
		},
	}
	for _, test := range tests {
		t.Run(test.name+"/"+string(test.domain), func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall(test.name, nil), RawReceipt{
				Structured: json.RawMessage(test.structured), ToolError: test.toolError,
			})
			if got.Transport != TransportSucceeded || got.Domain != test.domain || got.Evidence != test.evidence {
				t.Fatalf("verifier projection = %#v", got)
			}
		})
	}
}

func TestProjectReceiptToolErrorCannotBecomeVerifiedOrSuccessful(t *testing.T) {
	passedGlyph := `{"schemaVersion":1,"runId":"run-1","specName":"smoke","status":"passed","startedAt":"2026-07-13T00:00:00Z","endedAt":"2026-07-13T00:00:01Z","durationMs":1000,"target":{"cmd":["task","dev"]},"terminal":{"cols":80,"rows":24,"profile":"xterm-256color"},"outcomes":[{"id":"launch","status":"passed"}],"artifacts":{},"runDir":"/tmp/run-1","exitCode":0}`
	got := ProjectReceipt(ProjectToolCall("glyphrun__glyph_run", nil), RawReceipt{
		Structured: json.RawMessage(passedGlyph), ToolError: true,
	})
	if got.Successful() || got.Domain != DomainFailed || got.Evidence == EvidenceVerified {
		t.Fatalf("tool error was promoted by optimistic verifier envelope: %#v", got)
	}

	stored := ProjectReceipt(ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "cortex", "tool": "cortex_verify",
	}), RawReceipt{
		Structured: json.RawMessage(`{"status":"stored","callId":"call-123"}`), ToolError: true,
	})
	if stored.Successful() || stored.Domain == DomainSucceeded || stored.Evidence == EvidenceVerified {
		t.Fatalf("tool error was promoted by stored gateway receipt: %#v", stored)
	}
}

func TestProjectReceiptRejectsIncompleteVersionedVerifierEnvelopes(t *testing.T) {
	tests := []struct {
		name       string
		structured string
	}{
		{name: "glyph", structured: `{"schemaVersion":1,"status":"passed","exitCode":0}`},
		{name: "cairntrace", structured: `{"$schema":"urn:cairntrace.dev:run:v1","version":"1","status":"passed","exitCode":0}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall(test.name+"__"+test.name+"_run", nil), RawReceipt{
				Structured: json.RawMessage(test.structured),
			})
			if got.Domain != DomainUnknown || got.Evidence != EvidenceNone || got.Successful() {
				t.Fatalf("incomplete envelope was trusted: %#v", got)
			}
		})
	}
}

func TestProjectReceiptProjectsStaleCodemapAndStoredMCPHubResults(t *testing.T) {
	codemap := ProjectReceipt(ProjectToolCall("codemap__codemap_status", nil), RawReceipt{
		Structured: json.RawMessage(`{"registered":true,"indexed":true,"stale":{"changed":1,"new":0,"deleted":0}}`),
	})
	if codemap.Domain != DomainAttention || codemap.Evidence != EvidenceStale {
		t.Fatalf("codemap projection = %#v", codemap)
	}

	hub := ProjectReceipt(ProjectToolCall("mcphub__mcphub_call_tool", map[string]any{
		"server": "cortex", "tool": "cortex_verify",
	}), RawReceipt{Structured: json.RawMessage(`{"status":"stored","callId":"call-123","originalBytes":90000,"budgetBytes":8192}`)})
	if hub.Domain != DomainAttention || hub.Route.CallID != "call-123" || hub.Evidence != EvidenceNone {
		t.Fatalf("stored MCPHub projection = %#v", hub)
	}
}

func TestProjectReceiptTreatsTypedMCPErrorMetaAsDomainFailure(t *testing.T) {
	got := ProjectReceipt(ProjectToolCall("codemap__codemap_status", nil), RawReceipt{
		Text:      "status could not be loaded",
		ErrorMeta: json.RawMessage(`{"code":"index_unavailable","message":"missing"}`),
		ToolError: true,
	})
	if got.Transport != TransportSucceeded || got.Domain != DomainFailed || got.Evidence != EvidenceNone {
		t.Fatalf("typed MCP error projection = %#v", got)
	}
}

func TestProjectToolResultUsesEvidenceLadderWithoutInventingVerification(t *testing.T) {
	tests := []struct {
		name     string
		domain   DomainState
		evidence EvidenceState
	}{
		{"vecgrep__vecgrep_search", DomainUnknown, EvidenceNone},
		{"codemap__codemap_query", DomainUnknown, EvidenceNone},
		{"glyphrun__glyphrun_run", DomainUnknown, EvidenceNone},
		{"cairntrace__cairn_run", DomainUnknown, EvidenceNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectToolResult(ProjectToolCall(test.name, nil), `{}`, false)
			if got.Domain != test.domain || got.Evidence != test.evidence {
				t.Fatalf("projection = %#v", got)
			}
		})
	}
}

func TestProjectReceiptClassifiesTrustedExpertConsultationSummary(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		domain DomainState
	}{
		{name: "complete", text: "Expert consultation receipt (advisory; not verified evidence)\nexperts: total=2 · completed=2 · failed=0\nprivate expert prose", domain: DomainSucceeded},
		{name: "partial", text: "Expert consultation receipt (advisory; not verified evidence)\nexperts: total=3 · completed=2 · failed=1", domain: DomainAttention},
		{name: "all failed", text: "Expert consultation receipt (advisory; not verified evidence)\nexperts: total=2 · completed=0 · failed=2", domain: DomainFailed},
		{name: "no experts", text: "Expert consultation receipt (advisory; not verified evidence)\nexperts: total=0 · completed=0 · failed=0", domain: DomainFailed},
		{name: "malformed", text: "Expert consultation receipt (advisory; not verified evidence)\nexperts: total=3 · completed=3 · failed=1", domain: DomainUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectReceipt(ProjectToolCall("consult_experts", nil), RawReceipt{Text: test.text, TrustedLocal: true})
			if got.Transport != TransportSucceeded || got.Domain != test.domain || got.Evidence != EvidenceNone {
				t.Fatalf("expert projection = %#v", got)
			}
		})
	}

	untrusted := ProjectReceipt(ProjectToolCall("consult_experts", nil), RawReceipt{Text: "experts: total=1 · completed=1 · failed=0"})
	if untrusted.Domain != DomainUnknown || untrusted.Successful() {
		t.Fatalf("untrusted expert-shaped text was accepted: %#v", untrusted)
	}
}

func TestProjectionNormalizeBoundsAndRejectsUnknownEnums(t *testing.T) {
	projection := (ToolProjection{
		Specialist: strings.Repeat("x", 300) + "\x1b]52;c;payload",
		Operation:  "RUN BAD\nOP",
		Role:       "invented",
		Transport:  "invented",
		Domain:     "invented",
		Evidence:   "invented",
		Route:      ToolRoute{CallID: strings.Repeat("z", 300)},
	}).Normalize()
	if len(projection.Specialist) > maxProjectionIdentifierBytes || len(projection.Route.CallID) > maxProjectionIdentifierBytes {
		t.Fatalf("projection identifiers were not bounded: %#v", projection)
	}
	if projection.Role != RoleGeneral || projection.Transport != TransportSucceeded || projection.Domain != DomainUnknown || projection.Evidence != EvidenceNone {
		t.Fatalf("unknown enums survived normalization: %#v", projection)
	}
	if strings.ContainsAny(projection.Operation, "\n\x1b") {
		t.Fatalf("unsafe operation survived: %q", projection.Operation)
	}
}

func TestProjectionIdentifierBoundsRemainValidUTF8(t *testing.T) {
	projection := ProjectToolCall(strings.Repeat("界", maxProjectionIdentifierBytes)+"__glyph_run", nil)
	for name, value := range map[string]string{
		"specialist": projection.Specialist,
		"operation":  projection.Operation,
		"server":     projection.Route.Server,
		"tool":       projection.Route.Tool,
	} {
		if len(value) > maxProjectionIdentifierBytes || !json.Valid([]byte(`"`+value+`"`)) {
			t.Fatalf("%s was not UTF-8 bounded: %q (%d bytes)", name, value, len(value))
		}
	}
}
