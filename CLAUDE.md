# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

OpsNerva is a **local-first AI ops agent** written in Go (`internal/`) with a React + TypeScript frontend (`web/`). An LLM diagnoses, deploys and recovers Linux hosts over SSH through a fixed set of typed tools. The entire backend runs as one Go binary; the frontend is embedded into that binary. Release vehicles are Docker and Tauri desktop apps (Windows/Linux) that launch the Go binary as a sidecar.

The defining concern of the project is **safety**: all execution converges on a single `service.Service` entry point that runs validation → approval → encryption → audit in a fixed order, keeping the model, skills, remote output, and MCP clients outside the trusted computing base. When modifying behavior, preserve this trust boundary — do not let a model-provided host, connection, or command bypass it.

The interface text is Chinese-first (README, docs, and `web/src/locales/{en,zh}.ts` translations). Follow that when adding user-visible copy.

## Build, test, run

All commands run from the repo root. Requires Go 1.26+ and Node.js 22.13+. The `Makefile` fronts most tasks.

```bash
make dev-api   # Go backend only (listens :8080), after a `make build-web`
make dev-web   # Vite dev server (:5173), proxies /api to :8080
make test      # build-web + go test ./...
make check     # full verification: build-web + go test ./... + build-go
make build     # build web + produce bin/opsnerva
make test-web  # build-web only (type-check + vite build, the web "tests")
make clean     # rm -rf bin
```

- Run a single Go package's tests: `go test ./internal/service/...` — or call `run` from an editor in Go.
- Run one Go test by name: `go test ./internal/security/ -run TestRedact`.
- Web type-check/build (no test framework exists; `pnpm run build` runs `tsc --noEmit` + `vite build`): `pnpm --dir web run build`. During dev: `pnpm --dir web run dev`.
- Running the app: `./bin/opsnerva serve` (serves embedded frontend on `:8080`). Standalone commands: `host add|list|probe|scan-key|trust|delete`, `exec`, `approval`, `audit`, `chat`, `mcp`, `version`. No args = quick-start that creates `config.yaml`/`data/`/`workspace/` next to the binary.
- Desktop builds: **never compile, test, or build Rust/Tauri locally** (see AGENTS.md). The Go backend and web build are validated locally; Tauri packaging is validated only via the `.github/workflows/desktop.yml` GitHub Actions run.

Environment overrides used by config: `OPSNERVA_CONFIG`, `OPSNERVA_HOME`, `OPSNERVA_WORKSPACE_DIR`, `OPSNERVA_WORKSPACE_SANDBOX`, `OPSNERVA_LOG_LEVEL`, `OPSNERVA_DESKTOP`, plus `OPENAI_API_KEY/BASE_URL/MODEL` as a model fallback.

## Architecture

### Runtime bootstrap and command surface

`cmd/opsnerva/main.go` is the entry point. `newApplication` wires the stack: `store.Open` (SQLite) → `security.NewEncryptor` → `sshx.NewNativeSSHTransport` → `service.New` → workspace/skill/MCP init → `agent.New` (Eino ChatModelAgent runtime). The `serve` subcommand constructs the `httpapi.Server`. `version` constant lives in main.go.

### Packages (the request path)

The data flow is: Web/CLI/MCP → `internal/httpapi` / `internal/mcpserver` → `internal/agent` (Eino `ChatModelAgent`, typed tools) → `internal/service` (the trust boundary) → `internal/sshx` (in-process SSH). Persistence lives in `internal/store`; encryption + redaction in `internal/security`.

- `internal/domain` — shared request/response types (ExecRequest, Run, Approval, Host, ModelProvider, etc.) and prompts. Changes here ripple through every layer.
- `internal/service` (`service.go`) — **the only SSH executor**. Enforces the trust boundary in fixed order: resolves the target from SQLite by `host_id` (ignores model-supplied connection creds), normalizes/validates the request, binds a SHA-256 digest of payload+connection config, applies the approval mode, decrypts secrets only at execution time, then encrypts the raw request/output for audit. The Go `context` carries trusted values (session ID, workspace binding) that model tool arguments cannot forge.
- `internal/agent` — Eino `ChatModelAgent` runtime (`runtime.go`), ~39 typed tools (`tools.go`), message history, streaming events, and concurrency-safe hot-swap of the Runner when the active model provider changes. `reviewer.go` holds the two tool-less subagents (`ApprovalAgent` for manual explanations, `AutoApprovalAgent` for Auto mode) and `model_client.go` constructs the ChatModel per provider kind.
- `internal/store` (`store.go`) — schema lives in a single large `initializeSchema` const; new columns are added defensively via `ensureColumn` (ALTERs after create). SQLite uses `modernc.org/sqlite` (pure Go, CGO-free), single connection, WAL. Encryption at rest: secrets (API keys, private keys, passwords), command+output originals go into `*_cipher` columns as AES-256-GCM; the searchable/`*_redacted` columns hold only redacted views.
- `internal/security` — `crypto.go` (AES-256-GCM encryptor, master key), `redact.go` / `redact_stream.go` (uniform redaction of Authorization/Token/API key/private-key/cloud-credential patterns across requests, outputs, SSE, logs).
- `internal/sshx` — in-process SSH: auth (`ssh-agent` via Unix socket or Windows named pipe, uploaded private key, password), strict host-key checking, SFTP, SOCKS5/HTTP proxy, ProxyJump chains (max 4), tunnels, `go-pty` shells.
- `internal/httpapi` (`server.go`) — the loopback HTTP API + SSE + embedded React static assets. Routes are registered in `Server.routes()` using Go 1.22+ pattern routing (`GET /api/v1/...`). ServeMux middleware wrapper: `requestLogMiddleware` → `recoverMiddleware` → `corsMiddleware`.
- `internal/mcpserver` — the official MCP Go SDK adapters for both the stdio CLI (`opsnerva mcp`) and the Streamable HTTP endpoint (`/mcp`) that exposes the controlled SSH Service to MCP clients.
- `internal/config`, `internal/ids`, `internal/observability`, `internal/skills`, `internal/terminaltext`, `internal/proxyx` — config loading, ID generation, slog wiring, dynamic Skill registry, ANSI stripping, proxy HTTP.

### Frontend

`web/` is a Vite + React 19 + TypeScript SPA (i18next, react-markdown, xterm for terminals). Go embed is `web/embed.go` (embeds `web/dist`), so the frontend must be built before the Go binary serves it. Key files: `web/src/App.tsx` (root), `api.ts` (HTTP+SSE client), `types.ts`, `locales/{en,zh}.ts`. Tauri app lives in `web/src-tauri/`; the desktop build uses `web/scripts/build-desktop.mjs` to bundle the compiled Go sidecar.

## Security model (read before changing execution paths)

- **Approval modes** `Manual` / `Auto` / `Full access` all funnel through the same validation, connection binding, and encrypted-audit machinery. `Auto` delegates to an independent tool-less subagent that returns `allow/reject/manual`; invalid/unavailable/timeout falls back to user approval. Only a full `allow` executes. Refusing or requiring manual for a request must be the conservative default when anything is uncertain, unavailable, or malformed.
- **Digest binding**: a manual-approval summary binds host, command, args, env, and file content via SHA-256. Changing any field after approval fails execution.
- **Reduction**: remote output, tool args, SSE events, and logs are redacted before the model sees them; originals stay encrypted in SQLite. Never return `*_cipher`/decrypted content to the model or over HTTP except the deliberate raw-audit path.
- **Secrets**: sudo passwords and proxy passwords are decrypted only in memory at execution time; they never enter the request digest, audit JSON, or model tool args. API keys are stored encrypted and only `has_api_key` is returned.
- **External MCP servers**: tools from an admin-configured MCP server execute under that server's own privilege and do **not** inherit OpsNerva's approval; the UI must warn about this boundary. The built-in `/mcp` surface, by contrast, fully reuses validation + approval + audit.
- **Workspace**: paths are never returned to the model; SQLite stores only ID/permissions. `workspace_shell` is the only model-facing local shell; on Linux the Bubblewrap `sandbox` backend is the fail-safe default and never falls back to Host Shell.

## Conventions

- Keep frontend text concise (labels, values, statuses, actionable errors only); confirmation dialogs contain only title, required fields, buttons, and actionable errors. Delete unused locale keys/markup/styles when removing UI text. — AGENTS.md
- Both the Go backend and web must pass local checks for most edits (`make check`): the embedded frontend means a web TypeScript/type error fails the Go build's prerequisite.
- Comments and documentation in this repo are largely Chinese; match the surrounding style when editing near them (or keep English for code comments if that is the local file's existing convention).
- New model integrations route through `internal/agent/model_client.go` by provider `kind` (Anthropic → Claude component; everything else → OpenAI-compatible component).
- Shared proxy config is referenced by `proxy_id` from model providers, hosts, and web search; proxies in use are protected from deletion. Store behavior changes that are explicit, destructive migrations (e.g., the SSH transport-backend cleanup) delete legacy columns/tables rather than keeping dual-runtime branches.
