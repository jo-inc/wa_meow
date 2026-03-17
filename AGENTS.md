# jo-whatsapp Agent Guide

Go WhatsApp bridge running on Fly at :8090.

## Deploy
```bash
fly deploy
```

## Key Details
- Messages flow: jo-whatsapp → `whatsapp_events.py` (joprod) → mirror to macOS WS → `prepare_and_route_to_machine()`
- Response streaming: typing indicator on start, send text on `final` event.
- Single persistent CW per user (same pattern as Telegram).
