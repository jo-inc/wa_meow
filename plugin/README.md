# Jo WhatsApp OpenClaw Plugin

OpenClaw channel plugin for WhatsApp using the whatsmeow Go library.

## Architecture

```
┌─────────────────────────────────────────────────┐
│ OpenClaw Gateway (Node.js)                      │
│  └── Plugin: @jo/whatsapp                       │
│       └── HTTP/SSE client                       │
└────────────────────┬────────────────────────────┘
                     │ HTTP (send, status)
                     │ SSE (inbound messages)
                     ▼
┌─────────────────────────────────────────────────┐
│ wa_meow Server (Go + whatsmeow)             │
│  ├── POST /sessions         (create session)   │
│  ├── GET  /sessions/qr      (SSE QR stream)    │
│  ├── GET  /sessions/status  (connection status)│
│  ├── POST /messages/send    (send text)        │
│  ├── POST /messages/image   (send image)       │
│  ├── POST /messages/location(send location)    │
│  ├── POST /messages/react   (send reaction)    │
│  ├── POST /messages/typing  (typing indicator) │
│  └── GET  /events           (SSE message stream)│
└─────────────────────────────────────────────────┘
```

## Installation

### 1. Start the Go Server

```bash
cd wa_meow
go run ./cmd/server

# Or build and run
go build -o server ./cmd/server
./server
```

Environment variables:
- `PORT`: Server port (default: 8090)
- `DATA_DIR`: Directory for session databases (default: /data/whatsapp)
- `JO_BOT_URL`: Optional jo_bot URL for session persistence
- `WHATSAPP_SESSION_KEY`: Base64 encryption key for session backup

### 2. Install the Plugin

Copy the plugin to your OpenClaw extensions directory:

```bash
cp -r wa_meow/plugin ~/.openclaw/extensions/wa_meow
cd ~/.openclaw/extensions/wa_meow
npm install
```

Or add to `plugins.load.paths` in your OpenClaw config:

```yaml
plugins:
  load:
    paths:
      - /path/to/wa_meow/plugin
```

### 3. Configure the Channel

Add to your OpenClaw config:

```yaml
channels:
  wa_meow:
    serverUrl: http://localhost:8090
    accounts:
      main:
        userId: 1
        enabled: true
```

### 4. Pair WhatsApp

Use the OpenClaw CLI or UI to start the pairing wizard:

```bash
openclaw channels pair wa_meow --account main
```

Scan the QR code with WhatsApp on your phone.

## Features

- ✅ Send/receive text messages
- ✅ Send images with captions
- ✅ Send locations
- ✅ Reactions
- ✅ Typing indicators
- ✅ Multi-account support
- ✅ QR code pairing
- ✅ Session persistence
- ✅ Group chat support (info, participants)

## API Reference

### Config Schema

```typescript
interface JoWhatsAppConfig {
  serverUrl?: string;  // Default: http://localhost:8090
  accounts?: {
    [accountId: string]: {
      userId: number;      // User ID for the Go server
      enabled?: boolean;   // Default: true
    };
  };
}
```

### Capabilities

- Chat types: DM, Group
- Media: Images (more types coming)
- Reactions: ✅
- Typing indicators: ✅
- Threads: ❌
