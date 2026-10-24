"""Durable task protocol fixture. External effects use stable HTTP keys."""
import json
import pathlib
import sys
import threading
import time
import urllib.request

writer = threading.Lock()


def reply(identifier, result):
    with writer:
        sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": identifier, "result": result}) + "\n")
        sys.stdout.flush()


def effect(task, operation):
    url = task["input"].get("service")
    if url:
        request = urllib.request.Request(
            url + "/" + operation,
            data=json.dumps({"task": task["id"], "phase": task["phase"]}).encode(),
            headers={"Content-Type": "application/json", "Idempotency-Key": task["id"] + ":" + task["phase"] + ":" + operation},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=5) as response:
            if response.status != 200:
                raise RuntimeError("fake checkout failed")
            response.read()


def handle(frame):
    method = frame["method"]
    identifier = frame.get("id")
    if method == "initialize":
        reply(identifier, {"protocolVersion": "tempest-extension/1", "id": "tasks", "revision": "v1", "capabilities": ["tasks", "memo"], "definition": {"tasks": [{"name": "flow", "version": 1, "inputSchema": {"type": "object"}, "stateSchema": {"type": "object"}, "initial": {}}]}})
    elif method == "_tempest/extension/tasks/step":
        params = frame["params"]
        task = params["task"]
        if not params["context"]["token"] or params["context"]["attempt"] != task["attempt"]:
            raise RuntimeError("missing phase grant")
        if params.get("abort"):
            effect(task, "refund")
            reply(identifier, {"kind": "complete"})
            return
        mode = task["input"].get("mode", "reminder")
        if mode == "reminder":
            if task["phase"] == "run":
                reply(identifier, {"kind": "sleep", "phase": "delivery", "wakeAt": task["input"]["wakeAt"]})
            else:
                reply(identifier, {"kind": "complete", "submissions": [{"key": "delivery", "text": "durable reminder"}]})
        elif mode == "parent":
            if task["phase"] == "run":
                child = task["id"] + "-child"
                child_input = dict(task["input"], mode="child")
                reply(identifier, {"kind": "wait", "phase": "settled", "wait": [child], "failFast": True, "children": [{"id": child, "definition": "tasks/flow", "input": child_input}]})
            else:
                reply(identifier, {"kind": "complete"})
        elif mode == "child":
            effect(task, "charge")
            if "entered" in task["input"]:
                pathlib.Path(task["input"]["entered"]).write_text(str(task["attempt"]))
            if "release" in task["input"]:
                while not pathlib.Path(task["input"]["release"]).exists():
                    time.sleep(0.01)
            reply(identifier, {"kind": "complete"})
        else:
            raise RuntimeError("unknown fixture mode")
    elif method == "_tempest/extension/shutdown":
        reply(identifier, {})
    elif identifier is not None:
        raise RuntimeError("unexpected extension method " + method)


for line in sys.stdin:
    incoming = json.loads(line)
    threading.Thread(target=handle, args=(incoming,), daemon=True).start()
