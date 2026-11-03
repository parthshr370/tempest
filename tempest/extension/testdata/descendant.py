"""Extension that leaves a SIGTERM-ignoring child in its own process group.

argv: <mode> <pid file>. Mode "shutdown" starts the child when asked to shut
down, with no inherited standard streams, then answers and exits. Mode "exit"
starts the child after the handshake, holding the inherited stderr, and exits
without being asked.
"""
import json
import subprocess
import sys

# Longer than any test waits, so only a signal ends the child.
CHILD_LIFETIME_SECONDS = 600

CHILD = f"""
import signal, sys, time
signal.signal(signal.SIGTERM, signal.SIG_IGN)
print("ready", flush=True)
time.sleep({CHILD_LIFETIME_SECONDS})
"""

mode, pid_path = sys.argv[1], sys.argv[2]


def start_child(keep_stderr):
    """Starts the child and returns once its SIGTERM handler is installed."""
    child = subprocess.Popen(
        [sys.executable, "-c", CHILD],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=None if keep_stderr else subprocess.DEVNULL,
    )
    if child.stdout.readline() != b"ready\n":
        raise RuntimeError("child did not install its SIGTERM handler")
    child.stdout.close()
    with open(pid_path, "w", encoding="utf-8") as pid_file:
        pid_file.write(str(child.pid))


def answer(frame, result):
    print(json.dumps({"jsonrpc": "2.0", "id": frame["id"], "result": result}), flush=True)


for line in sys.stdin:
    frame = json.loads(line)
    method = frame.get("method")
    if method == "notifications/initialized":
        if mode == "exit":
            start_child(keep_stderr=True)
            sys.exit(0)
        continue
    if "id" not in frame:
        continue
    if method == "initialize":
        answer(frame, {"protocolVersion": "tempest-extension/1", "id": "descendant", "revision": "v1", "capabilities": [], "definition": {}})
    elif method == "_tempest/extension/shutdown":
        if mode == "shutdown":
            start_child(keep_stderr=False)
        answer(frame, {})
        sys.exit(0)
    else:
        raise RuntimeError(f"unexpected method {method}")
