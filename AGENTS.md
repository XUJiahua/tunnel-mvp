# Development instructions

Read [README.md](README.md), then [the development workflow](docs/agent-development-workflow.md).
Use the task's existing worktree and branch. Check `git status --short` before editing and preserve unrelated changes.

## Project map

- `cmd/tunnel`: one CLI binary, Server/Agent modes, storage maintenance and signed upgrades.
- `internal/control`: console/API, authorization, HTTP/WS/SSE gateway, Agent sessions and HA.
- `internal/agent`: outbound tunnel, DNS/IP/port ACL and diagnostics.
- `internal/state`: JSON, SQLite and PostgreSQL repositories, transactions, migrations and shared authentication.
- `web/app`: Next.js static console; `web/tests`: real-process Playwright acceptance.
- `third_party/remotedialer`: retained dependency patch; preserve its source and license.
- `docs/design` and corresponding `*-operations.md`: behavior contracts and operational boundaries.
- `scripts/preview*`, `scripts/dev.py`: development infrastructure; see the preview runbook.

## Commands and verification

- Toolchain: Go 1.27.2, Node 24+, Corepack and pnpm 12.14.0, Python 3.11+; development manager requires Linux `/proc` and `flock`.
- `make doctor`; `make install`; `make verify`. Root has no package.json; frontend commands execute in `web` with `corepack pnpm`.
- `make build` exports Next.js, copies assets to `internal/web/dist` and compiles Go. Runtime needs no Node.
- Use `python3 scripts/dev.py verify --profile infrastructure|browser|postgres|ha|upgrade|release` when applicable. See workflow for their coverage.
- Run relevant checks during implementation, then applicable delivery gates. Never call missing-service skips passes.
- Existing CI retains JSON/SQLite, PostgreSQL 15/18, real Keycloak, HA, upgrade and cross-platform gates. Keycloak is not provisioned by the new preview environment.

## Important boundaries

- Trace API, permissions, storage backend and active-stream revocation before changing behavior. Preserve organization isolation and failure-closed authentication.
- Preserve Agent DNS resolution/IP pinning, ACL enforcement and TLS verification. Do not add certificate bypasses for previews or tests.
- For repository changes read storage design/operations; for HA read cluster routing and HA contracts. JSON/SQLite remain single-active; shared files are not HA.
- Do not patch generated embed output. Change `web` then rebuild. A frontend dev server alone does not validate the shipped binary.
- `scripts/dev.py` serializes tests that currently use fixed ports. Use that entry across worktrees. Do not concurrently rewrite dependencies, `.next`, embed assets or a build directory.
- Never use production data, passwords or Agent tokens in previews. Only preview-owned synthetic state is managed automatically.

## Preview and delivery

- `make preview` allocates dev0–dev3; `make preview-local` explicitly uses loopback. `up` reuses a living worker; edits require `make preview-refresh`.
- Public console: `console-devN.cubbber.dev`; application namespace: `devN.app.cubbber.dev`. Both route to the same Server, ports 30500–30503.
- Independent main instance: `console.cubbber.dev`, `app.cubbber.dev`, port 30510. Only explicit `--main` from a clean main checkout manages it; task teardown does not touch it.
- Keep more specific worktree ingress rules before main `*.app.cubbber.dev`. Preserve Host. Certificates must cover each wildcard namespace.
- Stop editing before a final build. Deliver actual URL, build ID, commit/dirty state, checks and acceptance steps. `url` does not prove readiness; use `status`.
- Document incomplete or unavailable environment checks. Separate implementation, verification, user acceptance and release states.
- Commit only task files. Push, PR, merge and release remain within user authorization. Never treat preview readiness as release approval.
