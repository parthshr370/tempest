"""Standard-library peer used by Go integration tests and binary proofs."""
import json
import sys


class Peer:
    def __init__(self):
        self.sequence = 0

    def send(self, frame):
        sys.stdout.write(json.dumps(frame, separators=(",", ":")) + "\n")
        sys.stdout.flush()

    def callback(self, method, params):
        self.sequence += 1
        request_id = "callback-" + str(self.sequence)
        self.send({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params})
        while True:
            line = sys.stdin.readline()
            if not line:
                raise EOFError("host disconnected during callback")
            frame = json.loads(line)
            if frame.get("id") == request_id and "method" not in frame:
                if "error" in frame:
                    raise RuntimeError(frame["error"]["message"])
                return frame["result"]
            self.dispatch(frame)

    def dispatch(self, frame):
        method = frame.get("method", "")
        params = frame.get("params") or {}
        if "id" not in frame:
            return
        if method == "initialize":
            replay = "" if "--never" in sys.argv else "safe"
            result = {
                "protocolVersion": "tempest-extension/1", "id": "sample", "revision": "v1",
                "capabilities": ["read", "commit", "memo", "next"],
                "definition": {
                    "sections": [{"name": "instructions"}],
                    "tools": [{"name": "echo", "description": "Echo a message", "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"], "additionalProperties": False}, "outputSchema": {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]}, "effect": "write", "replay": replay}],
                    "hooks": [{"name": "approval", "event": "before_tool", "replay": "safe"}],
                    "wraps": [{"name": "brackets", "target": "sample/echo", "replay": "safe"}],
                    "documents": [{"namespace": "sample", "version": 1, "schema": {"type": "object", "properties": {"count": {"type": "integer"}}, "required": ["count"]}, "initial": {"count": 0}, "history": "rewindable", "fork": "asOf", "reset": "preserve"}]
                }
            }
        elif method.endswith("sections/render"):
            result = {"text": "Extension section"}
        elif method.endswith("hooks/call"):
            self.callback("_tempest/extension/context/memo", {"token": params["context"]["token"], "key": "approval", "candidate": True})
            result = {"kind": "continue"}
        elif method.endswith("tools/call"):
            identity = params["context"]
            if "--block" in sys.argv:
                marker_path = sys.argv[sys.argv.index("--block") + 1]
                with open(marker_path, "w", encoding="utf-8") as marker:
                    json.dump(identity, marker)
                # EOF releases the fixture after a killed host, without an orphan.
                for pending in sys.stdin:
                    json.loads(pending)
                return
            for key, candidate in [("false", False), ("null", None)]:
                self.callback("_tempest/extension/context/memo", {"token": identity["token"], "key": key, "candidate": candidate})
            self.callback("_tempest/extension/context/commit", {"token": identity["token"], "commit": {"id": identity["invocationId"] + ":document", "changes": [{"sessionId": identity["sessionId"], "kind": "document", "namespace": "sample", "id": "state", "version": 1, "value": {"count": 1}}]}})
            text = params["arguments"]["text"]
            result = {"content": [{"type": "text", "text": text}], "structuredContent": {"text": text}}
        elif method.endswith("wraps/call"):
            result = self.callback("_tempest/extension/context/next", {"token": params["context"]["token"]})
            result["content"][0]["text"] = "[" + result["content"][0]["text"] + "]"
            result["structuredContent"]["text"] = "[" + result["structuredContent"]["text"] + "]"
        elif method.endswith("shutdown"):
            result = {}
        else:
            self.send({"jsonrpc": "2.0", "id": frame["id"], "error": {"code": -32601, "message": "unknown method"}})
            return
        self.send({"jsonrpc": "2.0", "id": frame["id"], "result": result})

    def run(self):
        for line in sys.stdin:
            self.dispatch(json.loads(line))


Peer().run()
