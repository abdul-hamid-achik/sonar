package ui

import (
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

const maxToolPresentationWidth = 48

type toolActionLabels struct {
	running   string
	success   string
	attention string
	failure   string
}

type toolPresentation struct {
	label string
	raw   string
}

func (p toolPresentation) differsFromRaw() bool {
	return p.raw != "" && p.label != p.raw
}

var toolActionRegistry = map[string]toolActionLabels{
	"read":               {running: "Reading", success: "Read", failure: "Read failed"},
	"read_file":          {running: "Reading", success: "Read", failure: "Read failed"},
	"read_minified_file": {running: "Reading", success: "Read", failure: "Read failed"},
	"view":               {running: "Reading", success: "Read", failure: "Read failed"},
	"cat":                {running: "Reading", success: "Read", failure: "Read failed"},
	"write":              {running: "Writing", success: "Wrote", failure: "Write failed"},
	"write_file":         {running: "Writing", success: "Wrote", failure: "Write failed"},
	"create_file":        {running: "Creating", success: "Created", failure: "Create failed"},
	"edit":               {running: "Editing", success: "Edited", failure: "Edit failed"},
	"edit_file":          {running: "Editing", success: "Edited", failure: "Edit failed"},
	"apply_patch":        {running: "Patching", success: "Patched", failure: "Patch failed"},
	"patch":              {running: "Patching", success: "Patched", failure: "Patch failed"},
	"bash":               {running: "Running", success: "Ran", failure: "Run failed"},
	"exec":               {running: "Running", success: "Ran", failure: "Run failed"},
	"exec_command":       {running: "Running", success: "Ran", failure: "Run failed"},
	"shell":              {running: "Running", success: "Ran", failure: "Run failed"},
	"command":            {running: "Running", success: "Ran", failure: "Run failed"},
	"grep":               {running: "Searching", success: "Searched", failure: "Search failed"},
	"search":             {running: "Searching", success: "Searched", failure: "Search failed"},
	"search_files":       {running: "Searching", success: "Searched", failure: "Search failed"},
	"web_search":         {running: "Searching web", success: "Searched web", failure: "Web search failed"},
	"glob":               {running: "Finding", success: "Found", failure: "Find failed"},
	"find":               {running: "Finding", success: "Found", failure: "Find failed"},
	"ls":                 {running: "Listing", success: "Listed", failure: "List failed"},
	"list":               {running: "Listing", success: "Listed", failure: "List failed"},
	"list_directory":     {running: "Listing", success: "Listed", failure: "List failed"},
	"diff":               {running: "Comparing", success: "Compared", failure: "Compare failed"},
	"mkdir":              {running: "Creating directory", success: "Created directory", failure: "Create directory failed"},
	"make_directory":     {running: "Creating directory", success: "Created directory", failure: "Create directory failed"},
	"remove":             {running: "Removing", success: "Removed", failure: "Remove failed"},
	"delete":             {running: "Removing", success: "Removed", failure: "Remove failed"},
	"delete_file":        {running: "Removing", success: "Removed", failure: "Remove failed"},
	"copy":               {running: "Copying", success: "Copied", failure: "Copy failed"},
	"copy_file":          {running: "Copying", success: "Copied", failure: "Copy failed"},
	"move":               {running: "Moving", success: "Moved", failure: "Move failed"},
	"move_file":          {running: "Moving", success: "Moved", failure: "Move failed"},
	"rename":             {running: "Renaming", success: "Renamed", failure: "Rename failed"},
	"exists":             {running: "Checking", success: "Checked", failure: "Check failed"},
	"file_exists":        {running: "Checking", success: "Checked", failure: "Check failed"},
	"fetch":              {running: "Fetching", success: "Fetched", failure: "Fetch failed"},
	"fetch_url":          {running: "Fetching", success: "Fetched", failure: "Fetch failed"},
	"browse":             {running: "Browsing", success: "Browsed", failure: "Browse failed"},
	"memory_save":        {running: "Saving memory", success: "Saved memory", failure: "Save memory failed"},
	"memory_recall":      {running: "Recalling memory", success: "Recalled memory", failure: "Recall memory failed"},
	"memory_delete":      {running: "Removing memory", success: "Removed memory", failure: "Remove memory failed"},
	"memory_update":      {running: "Updating memory", success: "Updated memory", failure: "Update memory failed"},
	"memory_list":        {running: "Listing memories", success: "Listed memories", failure: "List memories failed"},
	"consult_experts":    {running: "Consulting experts", success: "Consulted experts", attention: "Expert consultation is partial", failure: "Expert consultation failed"},
	"update_plan":        {running: "Updating plan", success: "Updated plan", failure: "Update plan failed"},
	"tool_search":        {running: "Loading tools", success: "Loaded tools", failure: "Load tools failed"},
	"load_skill":         {running: "Loading skill", success: "Loaded skill", failure: "Skill load failed"},
	"task":               {running: "Running task", success: "Completed task", failure: "Task failed"},

	// Cortex keeps durable goals and evidence legible without exposing its MCP
	// implementation names in the compact transcript.
	"cortex_start_task":       {running: "Starting case", success: "Started case", failure: "Case start failed"},
	"cortex_open_task":        {running: "Opening case", success: "Opened case", failure: "Case open failed"},
	"cortex_investigate":      {running: "Investigating", success: "Investigated", failure: "Investigation failed"},
	"cortex_plan":             {running: "Planning change", success: "Planned change", failure: "Change plan failed"},
	"cortex_begin_change":     {running: "Claiming change", success: "Claimed change", failure: "Change claim failed"},
	"cortex_verify":           {running: "Verifying evidence", success: "Verified evidence", attention: "Verification needs review", failure: "Verification failed"},
	"cortex_remember":         {running: "Saving outcome", success: "Saved outcome", failure: "Outcome save failed"},
	"cortex_status":           {running: "Checking Cortex", success: "Checked Cortex", failure: "Cortex check failed"},
	"cortex_list_tasks":       {running: "Listing cases", success: "Listed cases", failure: "Case list failed"},
	"cortex_sessions":         {running: "Listing Cortex sessions", success: "Listed Cortex sessions", failure: "Session list failed"},
	"cortex_timeline":         {running: "Reading Cortex timeline", success: "Read Cortex timeline", failure: "Timeline read failed"},
	"cortex_metrics":          {running: "Reading Cortex metrics", success: "Read Cortex metrics", failure: "Metrics read failed"},
	"cortex_overview":         {running: "Reading Cortex overview", success: "Read Cortex overview", failure: "Overview read failed"},
	"cortex_archive":          {running: "Archiving case", success: "Archived case", failure: "Case archive failed"},
	"cortex_unarchive":        {running: "Restoring case", success: "Restored case", failure: "Case restore failed"},
	"cortex_resolve":          {running: "Resolving hypothesis", success: "Resolved hypothesis", failure: "Hypothesis resolution failed"},
	"cortex_note":             {running: "Recording evidence", success: "Recorded evidence", failure: "Evidence record failed"},
	"cortex_request_decision": {running: "Requesting decision", success: "Requested decision", failure: "Decision request failed"},
	"cortex_answer_decision":  {running: "Recording decision", success: "Recorded decision", failure: "Decision record failed"},
	"cortex_handoff":          {running: "Reading handoff", success: "Read handoff", failure: "Handoff read failed"},
	"cortex_abort_task":       {running: "Stopping case", success: "Stopped case", failure: "Case stop failed"},
	"cortex_read_evidence":    {running: "Reading evidence", success: "Read evidence", failure: "Evidence read failed"},
	"cortex_read_artifact":    {running: "Reading evidence artifact", success: "Read evidence artifact", failure: "Artifact read failed"},
	"cortex_recall_cases":     {running: "Recalling related cases", success: "Recalled related cases", failure: "Case recall failed"},

	// Bob exposes read-only inspection plus explicit repository-factory
	// actions. Use repository language so effects remain unmistakable.
	"bob_inspect":           {running: "Inspecting repository", success: "Inspected repository", attention: "Repository needs attention", failure: "Repository inspection failed"},
	"bob_context":           {running: "Reading repository context", success: "Read repository context", attention: "Repository context needs attention", failure: "Repository context failed"},
	"bob_path":              {running: "Classifying repository path", success: "Classified repository path", attention: "Repository path needs attention", failure: "Repository path classification failed"},
	"bob_plan":              {running: "Planning repository", success: "Planned repository", attention: "Plan needs attention", failure: "Repository plan failed"},
	"bob_playbook":          {running: "Reading repository playbook", success: "Read repository playbook", attention: "Repository playbook is blocked", failure: "Repository playbook failed"},
	"bob_check":             {running: "Checking repository contract", success: "Checked repository contract", attention: "Repository needs convergence", failure: "Repository check failed"},
	"bob_apply":             {running: "Applying repository plan", success: "Applied repository plan", failure: "Repository apply failed"},
	"bob_validate_manifest": {running: "Validating Bob manifest", success: "Validated Bob manifest", failure: "Manifest validation failed"},
	"bob_recipe_describe":   {running: "Reading Bob recipe", success: "Read Bob recipe", failure: "Recipe read failed"},
	"bob_stats":             {running: "Reading Bob stats", success: "Read Bob stats", failure: "Bob stats failed"},
	"bob_learn":             {running: "Learning Bob contract", success: "Learned Bob contract", failure: "Bob learn failed"},

	// Monitor has both observation and effectful tools. Action-specific copy
	// keeps a process termination from looking like a harmless generic call.
	"monitor_snapshot":        {running: "Reading system health", success: "Read system health", failure: "System health read failed"},
	"monitor_processes":       {running: "Listing processes", success: "Listed processes", failure: "Process list failed"},
	"monitor_doctor":          {running: "Checking diagnostic tools", success: "Checked diagnostic tools", failure: "Tool check failed"},
	"monitor_analyze":         {running: "Analyzing system health", success: "Analyzed system health", failure: "System analysis failed"},
	"monitor_kill":            {running: "Stopping process", success: "Stopped process", failure: "Process stop failed"},
	"monitor_profile_capture": {running: "Capturing process profile", success: "Captured process profile", failure: "Profile capture failed"},
	"monitor_investigate":     {running: "Investigating process", success: "Investigated process", failure: "Process investigation failed"},
	"monitor_record":          {running: "Recording screen", success: "Recorded screen", failure: "Screen recording failed"},

	// MCPHub management calls remain visibly different from the downstream
	// action summarized beside the card.
	"mcphub_list_servers":     {running: "Checking tool connections", success: "Checked tool connections", failure: "Connection check failed"},
	"mcphub_search_tools":     {running: "Searching ecosystem tools", success: "Searched ecosystem tools", failure: "Tool search failed"},
	"mcphub_describe_tool":    {running: "Reading tool contract", success: "Read tool contract", failure: "Tool contract read failed"},
	"mcphub_resolve_tool":     {running: "Resolving ecosystem tool", success: "Resolved ecosystem tool", failure: "Tool resolution failed"},
	"mcphub_call_tool":        {running: "Calling ecosystem tool", success: "Called ecosystem tool", failure: "Ecosystem tool failed"},
	"mcphub_get_result":       {running: "Reading stored result", success: "Read stored result", failure: "Stored result read failed"},
	"mcphub_stats":            {running: "Reading MCPHub stats", success: "Read MCPHub stats", failure: "MCPHub stats failed"},
	"hitspec_fetch":           {running: "Fetching HTTP content", success: "Fetched HTTP content", attention: "HTTP fetch needs review", failure: "HTTP fetch failed"},
	"hitspec_list_requests":   {running: "Listing saved HTTP requests", success: "Listed saved HTTP requests", attention: "Request list needs review", failure: "Request listing failed"},
	"hitspec_validate":        {running: "Validating HTTP request", success: "Validated HTTP request", attention: "HTTP request needs review", failure: "HTTP request validation failed"},
	"hitspec_search_web":      {running: "Searching the public web", success: "Found web candidates", attention: "Web search needs review", failure: "Web search failed"},
	"hitspec_capture_webpage": {running: "Capturing webpage artifact", success: "Captured webpage artifact", attention: "Webpage artifact needs review", failure: "Webpage capture failed"},

	// Discovery and structural evidence remain distinct from verification.
	"vecgrep_search":     {running: "Searching semantically", success: "Found candidates", attention: "Search needs attention", failure: "Semantic search failed"},
	"vecgrep_status":     {running: "Checking semantic index", success: "Checked semantic index", attention: "Semantic index needs attention", failure: "Semantic index check failed"},
	"vecgrep_index":      {running: "Indexing semantic search", success: "Indexed semantic search", attention: "Semantic index needs attention", failure: "Semantic indexing failed"},
	"codemap_status":     {running: "Checking code index", success: "Checked code index", attention: "Code index needs attention", failure: "Code index check failed"},
	"codemap_index":      {running: "Indexing code structure", success: "Indexed code structure", attention: "Code index needs attention", failure: "Code indexing failed"},
	"codemap_map":        {running: "Mapping code structure", success: "Mapped code structure", attention: "Code map needs attention", failure: "Code mapping failed"},
	"codemap_explore":    {running: "Exploring code structure", success: "Explored code structure", attention: "Code exploration needs attention", failure: "Code exploration failed"},
	"codemap_find":       {running: "Finding code symbol", success: "Found code symbol", attention: "Symbol lookup needs attention", failure: "Symbol lookup failed"},
	"codemap_context":    {running: "Reading structural context", success: "Read structural context", attention: "Structural context needs attention", failure: "Structural context failed"},
	"codemap_impact":     {running: "Tracing code impact", success: "Traced code impact", attention: "Impact analysis needs attention", failure: "Impact analysis failed"},
	"codemap_review":     {running: "Reviewing structural change", success: "Reviewed structural change", attention: "Structural review needs attention", failure: "Structural review failed"},
	"codemap_references": {running: "Tracing references", success: "Traced references", attention: "Reference evidence needs attention", failure: "Reference tracing failed"},

	// Verification tools only use the successful wording after their versioned
	// run envelope reports a passed outcome.
	"glyph_run":      {running: "Running terminal verification", success: "Verified terminal flow", attention: "Terminal verification needs review", failure: "Terminal verification failed"},
	"glyphrun_run":   {running: "Running terminal verification", success: "Verified terminal flow", attention: "Terminal verification needs review", failure: "Terminal verification failed"},
	"cairn_run":      {running: "Running browser verification", success: "Verified browser flow", attention: "Browser verification needs review", failure: "Browser verification failed"},
	"cairntrace_run": {running: "Running browser verification", success: "Verified browser flow", attention: "Browser verification needs review", failure: "Browser verification failed"},

	"vidtrace_extract":       {running: "Extracting video evidence", success: "Extracted video evidence", attention: "Video evidence needs attention", failure: "Video extraction failed"},
	"vidtrace_validate":      {running: "Validating video evidence", success: "Validated video evidence", attention: "Video evidence needs attention", failure: "Video validation failed"},
	"vidtrace_investigate":   {running: "Investigating video evidence", success: "Investigated video evidence", attention: "Video investigation is partial", failure: "Video investigation failed"},
	"fcheap_save":            {running: "Saving evidence artifact", success: "Saved evidence artifact", attention: "Artifact save needs review", failure: "Artifact save failed"},
	"fcheap_restore":         {running: "Restoring evidence artifact", success: "Restored evidence artifact", attention: "Artifact restore needs review", failure: "Artifact restore failed"},
	"fcheap_validate":        {running: "Validating evidence artifact", success: "Validated evidence artifact", attention: "Artifact validation needs review", failure: "Artifact validation failed"},
	"vault_get_secret":       {running: "Reading approved secret", success: "Read approved secret", attention: "Secret access needs attention", failure: "Secret read failed"},
	"vault_run_with_secrets": {running: "Running with scoped secrets", success: "Ran with scoped secrets", attention: "Secret-backed run needs attention", failure: "Secret-backed run failed"},
}

var toolKindRegistry = map[ToolCardKind]toolActionLabels{
	ToolCardFile:    {running: "Accessing file", success: "Accessed file", failure: "File operation failed"},
	ToolCardBash:    {running: "Running command", success: "Ran command", failure: "Command failed"},
	ToolCardSearch:  {running: "Searching", success: "Searched", failure: "Search failed"},
	ToolCardGit:     {running: "Updating repository", success: "Updated repository", failure: "Repository operation failed"},
	ToolCardGeneric: {running: "Running tool", success: "Completed tool", failure: "Tool failed"},
}

func presentTool(name string, kind ToolCardKind, state ToolCardState) toolPresentation {
	raw := safeToolIdentifier(name)
	labels, known := registeredToolLabels(raw)
	if !known && raw == "" {
		labels = toolKindRegistry[kind]
		known = true
	}

	var label string
	if known {
		label = labels.forState(state)
	} else {
		label = unknownToolLabel(humanizeToolIdentifier(raw), kind, state)
	}
	if label == "" {
		label = toolKindRegistry[ToolCardGeneric].forState(state)
	}

	return toolPresentation{
		label: truncateDisplay(label, maxToolPresentationWidth),
		raw:   raw,
	}
}

func (l toolActionLabels) forState(state ToolCardState) string {
	switch state {
	case ToolCardSuccess:
		return l.success
	case ToolCardError:
		return l.failure
	case ToolCardAttention:
		if l.attention != "" {
			return l.attention
		}
		// A transport can complete while the domain outcome still needs
		// interpretation. Never reuse the success verb for that state.
		return "Needs attention"
	default:
		return l.running
	}
}

func registeredToolLabels(raw string) (toolActionLabels, bool) {
	key := canonicalToolKey(raw)
	if labels, ok := toolActionRegistry[key]; ok {
		return labels, true
	}
	if namespace := strings.LastIndex(key, "__"); namespace >= 0 {
		labels, ok := toolActionRegistry[key[namespace+2:]]
		return labels, ok
	}
	return toolActionLabels{}, false
}

func canonicalToolKey(raw string) string {
	key := strings.ToLower(strings.TrimSpace(raw))
	return strings.NewReplacer("-", "_", " ", "_").Replace(key)
}

func unknownToolLabel(label string, kind ToolCardKind, state ToolCardState) string {
	if label == "" {
		return toolKindRegistry[kind].forState(state)
	}
	if state == ToolCardError {
		return label + " failed"
	}
	if state == ToolCardAttention {
		return label + " needs attention"
	}

	object := lowerInitial(label)
	switch kind {
	case ToolCardFile:
		if state == ToolCardSuccess {
			return "Accessed " + object
		}
		return "Accessing " + object
	case ToolCardBash:
		if state == ToolCardSuccess {
			return "Ran " + object
		}
		return "Running " + object
	case ToolCardSearch:
		if state == ToolCardSuccess {
			return "Searched with " + object
		}
		return "Searching with " + object
	case ToolCardGit:
		if state == ToolCardSuccess {
			return "Completed " + object
		}
		return "Running " + object
	default:
		if state == ToolCardSuccess {
			return "Completed " + object
		}
		return "Running " + object
	}
}

func safeToolIdentifier(name string) string {
	name = ansi.Strip(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '\ufe0e' || r == '\ufe0f' || r == '\u20e3':
			// Drop emoji presentation selectors and keycap composition marks.
		case unicode.IsLetter(r), unicode.IsNumber(r), unicode.IsMark(r), unicode.IsSpace(r):
			b.WriteRune(r)
		case strings.ContainsRune("_-./:@+", r) || r == '\\':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func humanizeToolIdentifier(raw string) string {
	words := strings.FieldsFunc(raw, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r)
	})
	if len(words) == 0 {
		return ""
	}
	label := strings.Join(words, " ")
	if label == strings.ToUpper(label) && label != strings.ToLower(label) {
		label = strings.ToLower(label)
	}
	return upperInitial(label)
}

func upperInitial(text string) string {
	runes := []rune(text)
	if len(runes) > 0 {
		runes[0] = unicode.ToUpper(runes[0])
	}
	return string(runes)
}

func lowerInitial(text string) string {
	runes := []rune(text)
	if len(runes) == 0 || firstWordIsInitialism(runes) {
		return text
	}
	runes[0] = unicode.ToLower(runes[0])
	return string(runes)
}

func firstWordIsInitialism(runes []rune) bool {
	letters := 0
	for _, r := range runes {
		if unicode.IsSpace(r) {
			break
		}
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if !unicode.IsUpper(r) {
			return false
		}
	}
	return letters > 1
}
