#!/bin/bash
# Register an existing LinqNumber with the local bridge and save its temporary
# pairing link to a private HTML file. The capability URL is never printed.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: $0 <linq-number-id> <e164-phone>" >&2
  exit 2
fi

cd "$(dirname "$0")/.."
export DATA_DIR="${DATA_DIR:-./data}"
export PORT="${PORT:-8090}"
TOKEN="${JO_WHATSAPP_ADMIN_TOKEN:-}"
if [[ -z "$TOKEN" && -r "$DATA_DIR/.admin-token" ]]; then
  TOKEN="$(<"$DATA_DIR/.admin-token")"
fi
if [[ -z "$TOKEN" ]]; then
  echo "Local bridge admin token is unavailable; start it with run-server.sh first." >&2
  exit 1
fi
export TOKEN

python3 - "$1" "$2" <<'PY'
import html, json, os, pathlib, sys, urllib.request
number_id, phone = int(sys.argv[1]), sys.argv[2]
port, token, root = os.environ["PORT"], os.environ["TOKEN"], pathlib.Path(os.environ["DATA_DIR"])
body = json.dumps({"id": number_id, "phone": phone}).encode()
request = urllib.request.Request(
    f"http://127.0.0.1:{port}/admin/linq-numbers", data=body,
    headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
)
with urllib.request.urlopen(request, timeout=10) as response:
    data = json.load(response)
path = data.get("pairing_url", "")
expected = f"/admin/linq-numbers/{number_id}/pair?token="
if data.get("status") != "registered" or not path.startswith(expected):
    raise SystemExit("bridge returned an invalid pairing link")
target = f"http://127.0.0.1:{port}{path}"
output = root / f"linq-number-{number_id}-pairing.html"
root.mkdir(mode=0o700, parents=True, exist_ok=True)
tmp = output.with_suffix(output.suffix + ".tmp")
with open(tmp, "w", encoding="utf-8") as f:
    os.fchmod(f.fileno(), 0o600)
    f.write("<!doctype html><meta name=referrer content=no-referrer><title>Jo WhatsApp pairing</title>")
    f.write(f'<p><a rel="noreferrer" href="{html.escape(target, quote=True)}">Open temporary Jo WhatsApp pairing</a></p>')
os.replace(tmp, output)
print(f"Private pairing link saved: {output}")
PY
