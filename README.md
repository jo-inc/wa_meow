# WhatsApp Bridge

A lightweight, self-hosted WhatsApp gateway for AI assistants. Built for [Moltbot](https://github.com/clawdbot/clawdbot) and similar personal AI agent projects.

Connect your AI assistant to WhatsApp in minutes. Send messages, receive events via SSE, and manage multiple sessions with a simple REST API.

## Why This Exists

If you're running [Moltbot](https://github.com/clawdbot/clawdbot) (formerly Clawdbot), [OpenClaw](https://docs.openclaw.ai), or building your own AI assistant, you need a way to connect to WhatsApp. This bridge:

- **Runs on your hardware** - Your messages stay with you
- **Simple REST API** - No complex protocols to learn
- **Real-time events** - SSE streaming for instant message delivery
- **Multi-user support** - One instance handles multiple WhatsApp accounts
- **Session persistence** - Encrypted backup/restore across restarts
- **More stable than Baileys** - Built on [whatsmeow](https://github.com/tulir/whatsmeow) (Go), which has better memory management and fewer session logout issues than the popular Baileys library

## Quick Start

### Option 1: Docker (Recommended)

```bash
docker run -d \
  --name wa_meow \
  -p 8090:8090 \
  -v wa_meow-data:/data/whatsapp \
  ghcr.io/jo-inc/wa_meow:latest
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
| `/messages/react` | POST | React to a message with emoji |
| `/messages/typing` | POST | Send typing indicator |
| `/chats?user_id=X` | GET | List all chats (contacts + groups) |
| `/events?user_id=X` | GET | SSE stream of incoming messages |

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

## Integrating with Moltbot

If you're using Moltbot/Clawdbot, point your WhatsApp channel configuration to this bridge:

```yaml
# In your Moltbot config
channels:
  whatsapp:
    type: wa_meow
    url: http://localhost:8090
    user_id: 1
```

The bridge handles all the WhatsApp protocol complexity. Your AI just sends/receives JSON.

## Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `DATA_DIR` | `/data/whatsapp` | SQLite database storage |
| `PORT` | `8090` | HTTP server port |
| `WHATSAPP_SESSION_KEY` | - | Base64 AES-256 key for encrypted session backup |

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

### Docker Compose

```yaml
version: '3.8'
services:
  wa_meow:
    image: ghcr.io/jo-inc/wa_meow:latest
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

- **No group chat support yet** - You can list groups and send messages to group JIDs, but group-specific features (mentions, replies, admin actions) are not implemented
- **Text messages only** - No media (images, audio, video, documents) support yet
- **No message history** - Only receives messages while connected

## Roadmap

We'd love your help! Here's what's planned:

- [ ] **Group chat support** - Mentions, replies, group metadata
- [ ] **Media messages** - Send and receive images, audio, video, documents
- [ ] **Message replies** - Quote and reply to specific messages
- [ ] **Read receipts** - Mark messages as read
- [ ] **Contact sync** - Better contact name resolution
- [ ] **Webhooks** - Push events to a URL instead of SSE
- [ ] **Authentication middleware** - Built-in API key auth

Want to contribute? Check out [CONTRIBUTING.md](CONTRIBUTING.md) and pick an item from the list above. PRs welcome!

## Credits

This project is built on **[whatsmeow](https://github.com/tulir/whatsmeow)** by [@tulir](https://github.com/tulir) - a robust, well-maintained Go library that handles all the WhatsApp Web protocol complexity. Without whatsmeow, this bridge wouldn't exist. If you find this project useful, consider starring [whatsmeow on GitHub](https://github.com/tulir/whatsmeow).

Inspired by the [Moltbot](https://github.com/clawdbot/clawdbot) community and the growing ecosystem of self-hosted AI assistants.

## License

MIT License - See [LICENSE](LICENSE) for details.
