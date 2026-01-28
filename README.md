# jo-whatsapp

WhatsApp bridge server for Jo, using whatsmeow. Runs on Fly.io.

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/health` | GET | Health check |
| `/sessions` | POST | Create/connect session (`user_id`) |
| `/sessions/qr?user_id=X` | GET | SSE stream of QR codes for login |
| `/sessions/status?user_id=X` | GET | Connection status |
| `/sessions/save?user_id=X` | POST | Save session to jo_bot |
| `/sessions/delete?user_id=X` | DELETE | Disconnect session |
| `/chats?user_id=X` | GET | List all chats |
| `/messages/send` | POST | Send message (`user_id`, `chat_jid`, `text`) |
| `/events?user_id=X` | GET | SSE stream of incoming messages |

## Local Development

```bash
./run-server.sh
```

Server runs on http://localhost:8090

## Environment Variables

| Variable | Description |
|----------|-------------|
| `DATA_DIR` | Local SQLite storage (default: `/data/whatsapp`) |
| `PORT` | Server port (default: `8090`) |
| `JO_BOT_URL` | Jo Bot API URL for session persistence |
| `WHATSAPP_SESSION_KEY` | Base64-encoded 32-byte AES key for encryption |

## Deployment

```bash
# Create the app (first time)
fly apps create jo-whatsapp

# Set the encryption key (generate with: openssl rand -base64 32)
fly secrets set WHATSAPP_SESSION_KEY="$(openssl rand -base64 32)"

# Deploy
fly deploy
```

## Usage Flow

1. Create session: `POST /sessions` with `{"user_id": 123}`
2. Get QR: `GET /sessions/qr?user_id=123` (SSE stream)
3. User scans QR with WhatsApp
4. On success, session is connected
5. List chats: `GET /chats?user_id=123`
6. Send message: `POST /messages/send` with `{"user_id": 123, "chat_jid": "...", "text": "..."}`
7. Receive messages: `GET /events?user_id=123` (SSE stream)

## Architecture

- Each user gets their own SQLite database: `/data/whatsapp/user_{id}.db`
- Sessions are loaded on-demand and kept in memory
- whatsmeow handles all WhatsApp protocol details
- Messages are pushed via SSE to Jo Bot for mirroring
