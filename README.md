# watui

WhatsApp in your terminal.

`watui` is a local-first CLI + TUI built on `whatsmeow`.

## Features
- Live sync with local SQLite storage
- Fast message search
- Terminal TUI for reading chats
- Send text and files
- Image preview from TUI (`p`, auto-download if needed)
- Contacts and groups management

## Build (local)
```bash
go build -tags sqlite_fts5 -o ./dist/watui ./cmd/watui
```

## Quick start
```bash
# 1) Login (QR)
./dist/watui auth

# 2) Open TUI (auto-sync starts)
./dist/watui tui
```

## Common commands
```bash
# Keep syncing in terminal
./dist/watui sync --follow

# Search messages
./dist/watui messages search "hello"

# Send text
./dist/watui send text --to 1234567890 --message "hi"

# Send file
./dist/watui send file --to 1234567890 --file ./photo.jpg

# Refresh contacts
./dist/watui contacts refresh
```

## Store
Default store: `~/.wacli`

Override with:
```bash
./dist/watui --store /path/to/store ...
```

## Notes
- Third-party client. Not affiliated with WhatsApp.
- Uses WhatsApp Web protocol via `whatsmeow`.
