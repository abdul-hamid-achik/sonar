PRAGMA foreign_keys = ON;

INSERT INTO sessions (
    id, title, model, mode, workspace_id, created_at, updated_at, public_id
) VALUES (
    41,
    'Recovery receipt review',
    'qwen3.5:2b',
    'BUILD',
    '__PROJECT_ROOT__',
    '2026-07-11T12:00:00.000Z',
    '2026-07-11T12:00:00.000Z',
    'c0ffee1'
);

INSERT INTO session_state (session_id, state_json, updated_at) VALUES (
    41,
    '{"version":1,"messages":[{"role":"user","content":"Review the release receipts."},{"role":"assistant","content":"I checked the saved verification receipts."}],"entries":[{"kind":"user","content":"Review the release receipts."},{"kind":"assistant","content":"I checked the saved verification receipts.","thinking_content":"The saved run includes one successful read and one failed test command.","thinking_collapsed":true},{"kind":"tool_group","tool_index":0},{"kind":"tool_group","tool_index":1},{"kind":"assistant","content":"The read completed, the test failure is visible, and the interrupted effect needs inspection."}],"tool_entries":[{"id":"fixture-read","name":"read_file","args":"path=internal/ui/view.go","result":"package ui\n\nfunc renderFixture() string { return \"ready\" }","result_language":"go","status":1,"start_time":"2026-07-11T12:00:00Z","duration":42000000,"collapsed":true},{"id":"fixture-bash","name":"bash","args":"command=go test ./internal/ui","result":"exit status 1: fixture regression","is_error":true,"status":2,"start_time":"2026-07-11T12:00:01Z","duration":310000000,"collapsed":true}],"mode":2,"model":"qwen3.5:2b","model_pinned":true,"session_eval_total":1234,"session_prompt_total":2560,"session_turn_count":1,"execution_cursor":0,"file_changes":{"internal/ui/view.go":1}}',
    '2026-07-11T12:00:00.000Z'
);

INSERT INTO execution_events (
    session_id,
    workspace_id,
    run_id,
    turn_id,
    execution_id,
    idempotency_key,
    provider_call_id,
    canonical_call_id,
    iteration,
    ordinal,
    tool_name,
    kind,
    effect_class,
    event_type,
    approval,
    arguments_sha256,
    result_sha256,
    result_receipt,
    detail,
    occurred_at
) VALUES (
    41,
    '__PROJECT_ROOT__',
    'fixture-run',
    'fixture-turn',
    'fixture-effect',
    'fixture-effect-key',
    'provider-fixture-effect',
    'fixture-effect-call',
    1,
    1,
    'write_file',
    'builtin',
    'effectful',
    'started',
    'once',
    '0000000000000000000000000000000000000000000000000000000000000000',
    '',
    '',
    'fixture process ended after dispatch',
    '2026-07-11T12:00:02Z'
);
