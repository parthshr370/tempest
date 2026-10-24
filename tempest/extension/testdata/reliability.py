"""Protocol fixture for call failures, retirement and wrapper budgets."""
import json
import os
import sys


def send(frame):
    print(json.dumps(frame), flush=True)


for line in sys.stdin:
    frame = json.loads(line)
    if "id" not in frame or "method" not in frame:
        continue
    method = frame["method"]
    params = frame.get("params") or {}
    if method == "initialize":
        result = {"protocolVersion": "tempest-extension/1", "id": "sample", "revision": "v1", "capabilities": ["next"], "definition": {"sections": [{"name": "s"}], "tools": [{"name": "t", "inputSchema": {"type": "object"}, "effect": "read", "replay": "safe"}], "wraps": [{"name": "w", "target": "builtin/read", "replay": "safe"}]}}
        if "--no-wrap" in sys.argv:
            result["definition"]["wraps"] = []
    elif method.endswith("sections/render"):
        result = {"text": "section ok"}
    elif method.endswith("tools/call"):
        mode = params["arguments"].get("mode")
        if mode == "block":
            continue
        if mode == "peer_cancel":
            send({"jsonrpc": "2.0", "method": "_tempest/extension/cancel", "params": {"token": params["context"]["token"], "invocationId": params["context"]["invocationId"]}})
            continue
        if mode == "exit":
            sys.exit(0)
        if mode == "error":
            send({"jsonrpc": "2.0", "id": frame["id"], "error": {"code": -32000, "message": "ordinary failure"}})
            continue
        if mode == "invalid":
            result = {"missing": "content"}
        else:
            result = {"content": [{"type": "text", "text": json.dumps(dict(os.environ)) if mode == "environment" else "ok"}]}
    elif method.endswith("wraps/call"):
        callback_id = "next-" + str(frame["id"])
        send({"jsonrpc": "2.0", "id": callback_id, "method": "_tempest/extension/context/next", "params": {"token": params["context"]["token"]}})
        for reply_line in sys.stdin:
            reply = json.loads(reply_line)
            if reply.get("id") == callback_id:
                result = reply.get("result") or {"content": [], "isError": True}
                break
    elif method.endswith("shutdown"):
        result = {}
    else:
        send({"jsonrpc": "2.0", "id": frame["id"], "error": {"code": -32601, "message": "unknown method"}})
        continue
    send({"jsonrpc": "2.0", "id": frame["id"], "result": result})
