#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT

cat >"$SCRATCH/faux.json" <<'JSON'
[
  {
    "text": "I will create the smoke file.",
    "toolCalls": [
      {"id":"smoke_write","name":"write","arguments":{"path":"app/page.tsx","content":"draft content\n"}}
    ]
  },
  {
    "text": "I will edit the smoke file.",
    "toolCalls": [
      {"id":"smoke_edit","name":"edit","arguments":{"path":"app/page.tsx","edits":[{"old_text":"draft","new_text":"final"}]}}
    ]
  },
  {"text":"Smoke completed."}
]
JSON

(cd "$ROOT" && go build -o "$SCRATCH/harness-smoke" ./cmd/harness)
(cd "$SCRATCH" && "$SCRATCH/harness-smoke" -faux-script "$SCRATCH/faux.json" -p "Create app/page.tsx then edit draft to final" >/dev/null)

if [[ ! -f "$SCRATCH/app/page.tsx" ]]; then
  echo "smoke: app/page.tsx was not created" >&2
  exit 1
fi

content="$(cat "$SCRATCH/app/page.tsx")"
if [[ "$content" != "final content" ]]; then
  echo "smoke: unexpected content: $content" >&2
  exit 1
fi

echo "smoke: ok"
