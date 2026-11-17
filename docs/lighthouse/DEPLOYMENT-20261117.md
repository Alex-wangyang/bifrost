# Lighthouse Bifrost RC4 deployment — 2026-11-17

Outcome: activated v2.5.1-lighthouse-r1-rc4 on Lighthouse. Runtime PID 1184335, accepted ELF SHA256 c22670ffc68488174508d4d38251c02d2d90ac51b8b050af570686f295d1593a. No production Core, OpenClaw or NASSH gateway changes.

Source merged locally at 9ebc5e9f4ea31ff2c6586312c90ad6e7bf870d69 and pushed to Alex-wangyang/bifrost lighthouse/v2.5.1, per owner's instruction to use local merge/push without an upstream PR. Native GitHub release lighthouse-v2.5.1-r1-rc4-20261117 (ID448441531) binds the exact source and accepted runtime; all four assets were independently downloaded by numeric ID and verified. The GitHub release immutable flag is false; hashes and tag identity were pinned and checked, not claimed GitHub-immutable. Hosted upstream CI was not run; exact merged source was rebuilt locally with complete selected suites and race checks, producing byte-identical ELF, then validated on native Linux/amd64.

## Stop, backup and activation

First stop hit the original15s timeout and SIGKILL. The guard refused activation; original b707 was restarted and its13healthy/1needs_reauth baseline recovered. Unknown external request outcomes from that stop remain UNKNOWN; nothing was replayed. The exact root cause is not established.

Second stop used a temporary90s runtime-only timeout override and exited normally (Resultsuccess, status0). Total stop+backup compression took8.951s, so this does not prove90s fixed the initial cause. The temporary override was removed and TimeoutStopUSec15s restored before activation.

The cold backup contains config/log SQLite state (including present sidecars), config.json, original unit/drop-ins and environment file. SHA2563650f758510634c40534482198f7330380c96eb53d9da7ea857d9cbf42fc9def;13files verified off-host, manifestSHA44459e8eaa4c9b2e29868a5117bc6d914d67dd73fb80147f9974a93ed2198290. Backup is private and is not a release asset.

Only a new final-priority Bifrost drop-in changes ExecStart's binary. App directory, bind address/port, logging arguments, environment references, original unit/drop-ins and config.json remain unchanged. Activation checks bind the current cold backup receipt and reject unit/env drift. Failure handling stops only this service and preserves new state; it never restores an old token database automatically.

## Post-activation evidence

- Actual process path/version/ELF match the published runtime. Service active, NRestarts0; memory about712MiB within the existing1GiB limit at readback.
-14MCP clients:13healthy; Airwallex remains needs_reauth, as before deployment.612tools, unchanged. No production Airwallex reauthorization was performed during cutover.
-10VK,31MCP grants,22provider configs,16provider-key mappings and1routing rule retain all original columns/rows exactly.
-3token identities preserved,2active/1needs_reauth; SQLite quick_check ok and0foreign-key errors. Invalid MCP credential returns401.
-Upstream startup cleanup removed10unreferenced pending OAuth configurations dating from August/September (15→5). These were not token rows; the remaining authorized/expired/revoked configs are retained. This is recorded expected cleanup, not zero database change.
-Native isolated old-binary/new-database test passed startup. Both candidate and rollback copies have zero WAL and identical logical/main schema+token digests. This is not a complete business rollback proof.

## Recovery boundary

Old b707 binary and original unit/drop-ins remain. Binary-only rollback removes/disables only the new RC4 drop-in after a controlled stop and a fresh snapshot of current state, then restarts the old binary retaining current databases and OAuth chain. Never blindly restore this pre-upgrade backup after token rotation or new effects. Full data recovery requires specific schema/state reconciliation. No rollback was performed after RC4 activation.

Real fresh read-only OAuth authorization/persistence/proactive refresh/restart/discovery passed in isolation. At cutover, production Airwallex still required login; the later authorization and renewal are recorded below. Revocation and natural business acceptance are not claimed. Historical upstream test exceptions remain documented. Owner production business acceptance is pending.

## Production Airwallex authorization and automatic renewal

After cutover, the owner completed the native authorization flow for the existing production Airwallex MCP client. Native complete-oauth returned HTTP 200 success; Airwallex recovered to healthy with 64 tools, and all 14 MCP clients were healthy. Access and refresh credentials were persisted encrypted. All 35 granted scopes match the existing OAuth configuration; no permission expansion was performed.

Airwallex rejected a registration attempt using the Tailnet HTTP callback with invalid_redirect_uri. The existing registration was retained and reauthorization used its registered localhost:28089 callback through a temporary SSH forward on the login workstation. That forward was closed after completion. No persistent callback proxy or gateway configuration change was introduced.

Read-only observation confirmed automatic production token renewal at 2026-11-17 18:34:43 +08:00: last_refreshed_at advanced from null and expiry advanced from 18:36:17 to 18:39:42 +08:00. At the 18:35:21 +08:00 token readback, the token remained active and encrypted, both credentials were present, and scopes still matched. Subsequent native client readback remained 14/14 healthy with 64 Airwallex tools. Service PID remained 1184335, NRestarts=0; no forced refresh or restart was used for this verification.

Sanitized evidence: [production-oauth-20261117.json](production-oauth-20261117.json). Credential values, authorization URLs, flow state and admin authentication are excluded. This closes the production authorization/persistence/first automatic renewal observation. Tool visibility and healthy state are not business-call acceptance; business operations and revocation remain NOT_RUN. Continued long-term stability remains normal operational observation.

This follow-up changes documentation only, with no runtime or wire-visible behavior change. Validation: live read-only OAuth/database and native client readback, evidence assertions, and git diff --check. Runtime tests were not rerun for this documentation-only update. Existing release tag and assets remain unchanged. The pre-upgrade database backup must not replace the now-rotated production OAuth chain.
