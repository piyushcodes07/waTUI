# AGENTS.md

This repository is the `wacli` WhatsApp CLI (Go). Use these local instructions when working here.

## Basics
- Default store: `~/.wacli` (override with `--store`).
- There are two SQLite DBs in the store dir:
  - `session.db` (whatsmeow session/auth state)
  - `wacli.db` (local app DB for chats/messages/contacts/groups)
- Many commands use `--json` to emit machine-readable output.

## Code Map
- CLI commands: `cmd/wacli`
- App wiring: `internal/app`
- WhatsApp client wrapper: `internal/wa`
- Local DB access + schema: `internal/store`
- Store locking: `internal/lock`
- Output formatting: `internal/out`

## Dev Notes
- Use `rg` to search.
- Avoid destructive git commands.
- Prefer small, scoped changes.

## Testing
- Go tests live in `internal/...` (use `go test ./...` when needed).
