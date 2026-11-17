# Lighthouse Bifrost 2.5.1 RC4 release contract

Owner: Alex-wangyang/bifrost, dedicated lighthouse/v2.5.1 release branch. Upstream transports/v2.5.1 commit 7cb75fb2f8276ca48d0792c857fecb67b8879e2f. This branch is independent of NASSH and upstream dev/main. The owner authorized local reviewed merge and push instead of an upstream PR.

## Source and build

The pinned source manifest records every tested source input, including go.work/go.work.sum and the vendored mcp-go fork. Generated embedded UI has its own 719-file manifest and is built from the unchanged upstream UI sources. Do not replace it with a placeholder. Build with Go1.27.3 on Linux/amd64, CGO musl static, netgo/osusergo/sqlite_static, -trimpath -buildvcs=false -buildid=. Exact command is build-rc4.sh (expects /out). No secrets or runtime databases belong in Git or a release.

Accepted ELF SHA256: c22670ffc68488174508d4d38251c02d2d90ac51b8b050af570686f295d1593a. Version v2.5.1-lighthouse-r1-rc4. Accepted runtime archive 85b41779d4db308d1da39212d41cfe24c5c57ccc8725e7a37fe4dbfa76a84dec and source archive 8b1a7fadcac8a5aed2ba5eea63ccfe11ac04badeb3098bf0820f7454a1a2b28a. Keep historical build manifests unchanged; bind these inputs to the accepted merge commit in an additional release receipt.

Publication uses native Git/GitHub Release operations in this fork, with a unique tag, exact merge SHA, manifest/checksums and independent downloaded-asset verification. No upstream npm/Docker multi-package release pipeline is invoked. Local/isolated evidence and upstream CI are distinct; do not claim upstream hosted CI passed if not run.

## Validation

Completed: fidelity and OAuth persistence regression tests, controlled_connectors case/duplicate rejection, deterministic config/counter concurrency (20 repeats), selected governance race (5 repeats), full governance/core schemas/HTTP lib suites, gateway refusal regression (20 repeats), 14 policy artifact negatives, complete UI asset hashes, 11 browser checks, 3 startup faults, unpacked runtime, cached/fresh/restarted catalog and cold recovery rehearsal. Real Airwallex new authorization, encrypted persistence, restart, proactive refresh and tool discovery passed with two read-only scopes; no business tools were called. Revocation and old production token migration are not implied. Historical upstream SDK OAuth convenience-test and German currency-format-test exceptions remain recorded; no all-repository-green claim.

Rate-limit publication runs only on config updates; request counter hot paths are unchanged. Retained fidelity data is bounded by individual discovered tool/result JSON; SDK raw fields preserve unknown fields and integer precision. OAuth retries only persistence (three writes), never repeat the token exchange. Caller/owner/config map snapshots are detached.

## Target and staging

Lighthouse Linux/amd64, user service shinghome-bifrost.service. Persistent app directory /home/ubuntu/.local/share/shinghome-bifrost. Existing environment file and all unrelated drop-ins are preserved. Install fixed bytes into a NEW directory under /home/ubuntu/.local/share/shinghome-bifrost-release. No production build or dependency install. Native platform validation must use a separate app directory and network namespace before activation; it cannot call production MCPs.

## Cutover and recovery

Refresh process/ELF/config/controlled_connectors/OAuth/SQLite and shared deployment window first. Preserve unit, drop-ins, environment references and permissions privately. Stop only shinghome-bifrost.service (bounded graceful stop); preserve a consistent final backup including SQLite WAL sidecars and verify it off-host. Do not replay uncertain in-flight requests. A new final-priority drop-in changes only ExecStart to the accepted immutable binary, retaining app directory, host, port and logging arguments; daemon-reload/start only this service. Re-read actual process ELF, version, database checks, grants and MCP state. Catalog/permission checks are separate from natural business success.

Rollback target: b707 binary f110e29e4cd6240f78006c0ca969de1e4197f8c06350522b63ac46b577fdd2a5 and original unit/drop-ins. Test old-code compatibility with the upgraded database in isolation first. Prefer binary-only rollback retaining current OAuth chain; never blindly restore a pre-refresh database. If schema/data incompatibility is detected, stop and preserve post-change state for explicit recovery instead of overwriting new tokens/UNKNOWN outcomes. Do not change OpenClaw, Core, NASSH, downstream service grants, or business settings.
