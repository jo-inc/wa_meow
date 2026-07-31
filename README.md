# WhatsApp Bridge

A lightweight, self-hosted WhatsApp gateway for AI assistants. Built for [OpenClaw](https://openclaw.ai) and similar personal AI agent projects.

> **Production-tested:** This code is extracted from and powers the WhatsApp integration at [askjo.ai](https://askjo.ai?ref=wa_meow).

## Add to OpenClaw

```bash
openclaw plugins install @askjo/wa_meow
```

Or directly from GitHub:

```bash
openclaw plugins install https://github.com/jo-inc/wa_meow
```

Then configure:

```yaml
channels:
  wa_meow:
    serverUrl: http://localhost:8090
    accounts:
      main:
        userId: 1
        enabled: true
```

## Release Channels

This repo ships two different artifacts:

- **Fly / Docker deploy** — runs the Go HTTP bridge from `cmd/server/`.
- **npm / GitHub plugin install** — ships the OpenClaw plugin from `src/` compiled into `dist/`, plus the bundled `bin/` binaries.

That means:

- Changes under `cmd/server/` need a Fly deploy.
- Changes under `src/`, `package.json`, or `openclaw.plugin.json` need a new npm package release.
- If a change touches both surfaces, release both.

---

Connect your AI assistant to WhatsApp in minutes. Send messages, receive events via SSE, and manage multiple sessions with a simple REST API.

## Why This Exists

If you're running [OpenClaw](https://openclaw.ai) or building your own AI assistant, you need a way to connect to WhatsApp. This bridge:

- **Runs on your hardware** - Your messages stay with you
- **Simple REST API** - No complex protocols to learn
- **Real-time events** - SSE streaming for instant message delivery
- **Multi-user support** - One instance handles multiple WhatsApp accounts
- **Session persistence** - Encrypted backup/restore across restarts
- **More stable than Baileys** - Built on [whatsmeow](https://github.com/tulir/whatsmeow) (Go), which has better memory management and fewer session logout issues than the popular Baileys library

## Quick Start

### Option 1: Docker (Recommended)

```bash
# Build the image locally
git clone https://github.com/jo-inc/wa_meow.git
cd wa_meow
docker build -t wa_meow .

# Run it
docker run -d \
  --name wa_meow \
  -p 8090:8090 \
  -v wa_meow-data:/data/whatsapp \
  wa_meow
```

### Option 2: From Source

```bash
# Clone the repo
git clone https://github.com/jo-inc/wa_meow.git
cd wa_meow

# Run (requires Go 1.21+)
./run-server.sh
```

### Connect to WhatsApp

```bash
# 1. Create a session
curl -X POST localhost:8090/sessions -d '{"user_id": 1}'

# 2. Get QR code (opens SSE stream)
curl localhost:8090/sessions/qr?user_id=1

# 3. Scan QR with your phone (WhatsApp > Linked Devices > Link a Device)

# 4. You're connected! Send a message:
curl -X POST localhost:8090/messages/send \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1, "chat_jid": "1234567890@s.whatsapp.net", "text": "Hello from my AI!"}'
```

## API Reference

The bridge accepts only direct-user chat JIDs (`s.whatsapp.net`, `lid`, and legacy `c.us`). Group, broadcast/status, newsletter, bot, hosted, and interoperability JIDs are rejected.

### Sessions

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/sessions` | POST | Create session (`{"user_id": 123}`) |
| `/sessions/qr?user_id=X` | GET | SSE stream of QR codes for login |
| `/sessions/status?user_id=X` | GET | Connection status |
| `/sessions/save?user_id=X` | POST | Persist session (requires encryption key) |
| `/sessions/delete?user_id=X` | DELETE | Disconnect and cleanup |

### Messages

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/messages/send` | POST | Send text message |
| `/messages/audio` | POST | Send audio/voice message |
| `/messages/document` | POST | Send document (PDF, etc.) |
| `/messages/react` | POST | React to a message with emoji |
| `/messages/typing` | POST | Send typing indicator |
| `/events?user_id=X` | GET | SSE stream of incoming messages |
| `/media/download` | POST | Download media from a message |

### Health

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Health check |

## Message Format

### Send a Message

```bash
curl -X POST http://localhost:8090/messages/send \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": 1,
    "chat_jid": "1234567890@s.whatsapp.net",
    "text": "Hello!"
  }'
```

### React to a Message

```bash
curl -X POST http://localhost:8090/messages/react \
  -H "Content-Type: application/json" \
  -d '{
    "user_id": 1,
    "chat_jid": "1234567890@s.whatsapp.net",
    "message_id": "ABC123",
    "emoji": "thumbsup"
  }'
```

### Listen for Incoming Messages

```bash
curl -N http://localhost:8090/events?user_id=1
```

Events are delivered as SSE:

```
event: message
data: {"type":"message","payload":{"id":"ABC123","chat_jid":"1234567890@s.whatsapp.net","sender_jid":"9876543210@s.whatsapp.net","sender_name":"John","text":"Hey there!","timestamp":1706745600,"is_from_me":false}}
```

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `DATA_DIR` | `/data/whatsapp` | SQLite database storage |
| `PORT` | `8090` | HTTP server port |
| `WHATSAPP_SESSION_KEY` | - | Base64 AES-256 key for encrypted session backup |
| `JO_BOT_URL` | - | Callback URL for session persistence |
| `JO_WHATSAPP_INTERNAL_TOKEN` | - | Auth token for session API calls |
| `SENTRY_DSN` | - | Sentry DSN for error tracking (optional) |

### Session Encryption (Optional)

To persist sessions across container restarts or sync between instances:

```bash
# Generate a key
openssl rand -base64 32

# Set it as an environment variable
export WHATSAPP_SESSION_KEY="your-generated-key"
```

## Deployment

### Fly.io

```bash
fly apps create my-wa_meow
fly secrets set WHATSAPP_SESSION_KEY="$(openssl rand -base64 32)"
fly deploy
```

### npm plugin release

```bash
npm version patch
npm publish
```

The package `prepare` step builds `src/` into `dist/`, so both npm publishes and direct GitHub installs include the compiled plugin files.

### Docker Compose

```yaml
version: '3.8'
services:
  wa_meow:
    build: .
    ports:
      - "8090:8090"
    volumes:
      - wa_meow-data:/data/whatsapp
    environment:
      - WHATSAPP_SESSION_KEY=your-secret-key
    restart: unless-stopped

volumes:
  wa_meow-data:
```

## Architecture

```
Your AI Assistant
       |
       | REST API (JSON)
       v
+------------------+
| WhatsApp Bridge  |  <-- This project
+------------------+
       |
       | WhatsApp Web Protocol (via whatsmeow)
       v
+------------------+
| WhatsApp Servers |
+------------------+
```

- **One SQLite database per user** - Sessions are isolated
- **whatsmeow** - Battle-tested WhatsApp Web client library
- **Server-Sent Events** - Real-time message streaming
- **AES-256-GCM** - Optional session encryption for backup

## Development

```bash
# Install air for live-reload
go install github.com/air-verse/air@latest

# Run with auto-reload
./run-server.sh
```

## Troubleshooting

**QR code not appearing?**
- Make sure you're using `curl -N` to disable buffering
- The QR stream times out after 2 minutes

**Session keeps disconnecting?**
- WhatsApp may disconnect linked devices after 14 days of phone inactivity
- Keep your phone connected to the internet

**Getting rate limited?**
- WhatsApp has sending limits. Space out your messages.
- Don't spam or you'll get banned.

## Security Notes

- This connects to your personal WhatsApp account
- Your session data is stored locally (or encrypted if `WHATSAPP_SESSION_KEY` is set)
- Never expose this bridge to the public internet without authentication
- Consider running behind a reverse proxy with auth

## Current Limitations

- **Direct chats only** - Groups, broadcasts/status, newsletters, bots, hosted/interoperability chats, and historical chat synchronization are intentionally disabled
- **No message history** - Only receives messages while connected

## Credits

This project is built on **[whatsmeow](https://github.com/tulir/whatsmeow)** by [@tulir](https://github.com/tulir) - a robust, well-maintained Go library that handles all the WhatsApp Web protocol complexity. Without whatsmeow, this bridge wouldn't exist. If you find this project useful, consider starring [whatsmeow on GitHub](https://github.com/tulir/whatsmeow).

Inspired by the [OpenClaw](https://openclaw.ai) community and the growing ecosystem of self-hosted AI assistants.

## License

MIT License - See [LICENSE](LICENSE) for details.
