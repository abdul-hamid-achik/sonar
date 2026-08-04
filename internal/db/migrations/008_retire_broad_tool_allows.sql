-- Exact-request session approvals supersede the historical per-tool "allow"
-- policy. A broad row cannot prove the workspace, canonical arguments or
-- content digest the user reviewed, so upgrades must fail closed to "ask".
UPDATE tool_permissions
   SET policy = 'ask',
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
 WHERE policy = 'allow';
