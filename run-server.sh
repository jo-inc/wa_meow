#!/bin/bash
# Run the WhatsApp server locally, connected to local jo_bot

cd "$(dirname "$0")"

export DATA_DIR="${DATA_DIR:-./data}"
export PORT="${PORT:-8090}"
export LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:$PORT}"
export JO_BOT_URL="${JO_BOT_URL:-http://localhost:10000}"
SERVER_BIN="${SERVER_BIN:-./tmp/server}"
BUILD_SERVER="${BUILD_SERVER:-1}"

# Fixed dev key for local development (32 bytes base64-encoded)
# In production: fly secrets set WHATSAPP_SESSION_KEY="$(openssl rand -base64 32)"
export WHATSAPP_SESSION_KEY="${WHATSAPP_SESSION_KEY:-MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=}"

mkdir -p "$DATA_DIR"
mkdir -p "./tmp"

# Reuse the private local bridge/backend credential across manual restarts.
if [ -z "${JO_WHATSAPP_INTERNAL_TOKEN:-}" ] && [ -s "$DATA_DIR/.internal-token" ]; then
  export JO_WHATSAPP_INTERNAL_TOKEN="$(<"$DATA_DIR/.internal-token")"
fi

# Keep local admin registration enabled without a shared/default credential.
# The generated bearer secret stays in a mode-0600 file under the ignored data dir.
if [ -z "${JO_WHATSAPP_ADMIN_TOKEN:-}" ]; then
  ADMIN_TOKEN_FILE="$DATA_DIR/.admin-token"
  if [ ! -s "$ADMIN_TOKEN_FILE" ]; then
    TOKEN_TMP="$ADMIN_TOKEN_FILE.$$"
    (umask 077; openssl rand -hex 32 > "$TOKEN_TMP") || exit 1
    mv "$TOKEN_TMP" "$ADMIN_TOKEN_FILE"
  fi
  chmod 600 "$ADMIN_TOKEN_FILE"
  export JO_WHATSAPP_ADMIN_TOKEN="$(<"$ADMIN_TOKEN_FILE")"
fi

echo "🚀 WhatsApp server: http://localhost:$PORT"
echo "📁 Data: $DATA_DIR"
echo "🔗 Jo Bot: $JO_BOT_URL"
echo ""
echo "Test:"
echo "  curl -X POST localhost:$PORT/sessions -d '{\"user_id\":195}'"
echo "  curl localhost:$PORT/sessions/status?user_id=195"
echo ""

# Build and run (logs visible)
if [ "$BUILD_SERVER" = "1" ]; then
  echo "🔨 Building..."
  go build -o "$SERVER_BIN" ./cmd/server || exit 1
elif [ ! -x "$SERVER_BIN" ]; then
  echo "Prebuilt server binary is not executable: $SERVER_BIN" >&2
  exit 1
fi
echo "✅ Running server..."
exec "$SERVER_BIN" 2>&1
