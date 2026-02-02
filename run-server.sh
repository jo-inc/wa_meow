#!/bin/bash
# Run the WhatsApp server locally, connected to local jo_bot

cd "$(dirname "$0")"

export DATA_DIR="${DATA_DIR:-./data}"
export PORT="${PORT:-8090}"
export JO_BOT_URL="${JO_BOT_URL:-http://localhost:10000}"

# Fixed dev key for local development (32 bytes base64-encoded)
# In production: fly secrets set WHATSAPP_SESSION_KEY="$(openssl rand -base64 32)"
export WHATSAPP_SESSION_KEY="${WHATSAPP_SESSION_KEY:-MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=}"

mkdir -p "$DATA_DIR"
mkdir -p "./tmp"

echo "🚀 WhatsApp server: http://localhost:$PORT"
echo "📁 Data: $DATA_DIR"
echo "🔗 Jo Bot: $JO_BOT_URL"
echo ""
echo "Test:"
echo "  curl -X POST localhost:$PORT/sessions -d '{\"user_id\":195}'"
echo "  curl localhost:$PORT/sessions/status?user_id=195"
echo ""

# Use air for live-reload if available, otherwise go run
if command -v air &> /dev/null; then
    air
else
    echo "💡 Install air for live-reload: go install github.com/air-verse/air@latest"
    go run ./cmd/server
fi
