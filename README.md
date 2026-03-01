# watui

A fast WhatsApp CLI + TUI built on top of `whatsmeow`.

## What it does
- Syncs messages into a local SQLite store
- Search, list, and send messages
- Read‑only TUI with live sync

> This is a third‑party client using WhatsApp Web protocol. Not affiliated with WhatsApp.

## Build
```bash
go build -tags sqlite_fts5 -o ./dist/watui ./cmd/watui
```

## Quick start
```bash
# Authenticate (QR)
./dist/watui auth

# Start TUI (auto sync in background)
./dist/watui tui

# Keep syncing without TUI
./dist/watui sync --follow
```

## Useful commands
```bash
# Search messages
./dist/watui messages search "meeting"

# Send text
./dist/watui send text --to 1234567890 --message "hello"

# Send file
./dist/watui send file --to 1234567890 --file ./pic.jpg
```

## Storage
Default store is `~/.wacli` (override with `--store DIR`).

## License
MIT
