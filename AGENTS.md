# jo-whatsapp Agent Guide

This file supplements `../AGENTS.md`; both apply. If guidance conflicts, follow the safer or more specific rule and flag the conflict.

Go WhatsApp bridge running on Fly at :8090.

## Safety
- Never send, react to, delete, or otherwise mutate a user's WhatsApp conversation without explicit approval; use only an explicitly designated test account for live tests.
- Never log or commit tokens, webhook secrets, message bodies, phone numbers, media, or other personal data. Sanitize diagnostics and fixtures.
- Treat webhook retries and reconnects as normal: processing and delivery must be deduplicated and idempotent, with read-back/recovery where acknowledgements can be lost.

## Testing
- Add regression tests for duplicate events, reconnect/retry behavior, partial delivery, formatting, and media handling as relevant.
- Verify webhook authentication and the full bridge → joprod → user machine → final response path; a successful handler status alone does not prove delivery.

## Deploy
Deployment requires explicit approval for `jo-whatsapp` production. After an approved deploy, monitor completion and verify health plus one approved canary conversation.
```bash
fly deploy
```

## Key Details
- Messages flow: jo-whatsapp → `whatsapp_events.py` (joprod) → mirror to macOS WS → `prepare_and_route_to_machine()`
- Response streaming: typing indicator on start, send text on `final` event.
- Single persistent CW per user (same pattern as Telegram).
