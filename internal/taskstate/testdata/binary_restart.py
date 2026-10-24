"""Run with a built tempest binary and the repository root. No provider key needed."""
import datetime
import http.server
import json
import os
import pathlib
import queue
import signal
import subprocess
import sys
import tempfile
import threading
import time

binary = pathlib.Path(sys.argv[1]).resolve()
repository = pathlib.Path(sys.argv[2]).resolve()


class Peer:
    def __init__(self, workspace, config, script, sessions):
        environment = {key: value for key, value in os.environ.items() if not key.startswith(("HARNESS_", "LYRA_"))}
        environment["HOME"] = str(workspace)
        self.process = subprocess.Popen([str(binary), "serve", "-session-dir", str(sessions), "-faux-script", str(script), "-extension-config", str(config), "-mcp-config", "", "-permission-mode", "bypass"], cwd=workspace, env=environment, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
        self.responses = queue.Queue()
        self.sequence = 0
        self.pending = {}
        threading.Thread(target=self.read, daemon=True).start()
        initialize = self.call("initialize", {"protocolVersion": 1})
        assert initialize["agentCapabilities"]["_meta"]["tempest"]["durable"]["tasks"]

    def read(self):
        for line in self.process.stdout:
            frame = json.loads(line)
            if "id" in frame:
                self.responses.put(frame)

    def send(self, method, params):
        self.sequence += 1
        self.process.stdin.write(json.dumps({"jsonrpc": "2.0", "id": self.sequence, "method": method, "params": params}) + "\n")
        self.process.stdin.flush()
        return self.sequence

    def receive(self, identifier):
        deadline = time.monotonic() + 15
        while identifier not in self.pending:
            response = self.responses.get(timeout=max(0.01, deadline - time.monotonic()))
            self.pending[response["id"]] = response
        response = self.pending.pop(identifier)
        if "error" in response:
            raise RuntimeError(response["error"])
        return response.get("result")

    def call(self, method, params):
        return self.receive(self.send(method, params))

    def crash(self):
        os.killpg(self.process.pid, signal.SIGKILL)
        self.process.wait(timeout=5)
        self.process.stdin.close()
        self.process.stdout.close()
        self.process.stderr.close()

    def close(self):
        if self.process.poll() is None:
            self.process.send_signal(signal.SIGTERM)
            self.process.communicate(timeout=10)


def wait_until(predicate):
    deadline = time.monotonic() + 10
    while not predicate():
        if time.monotonic() > deadline:
            raise RuntimeError("fixture boundary not reached")
        time.sleep(0.01)


with tempfile.TemporaryDirectory(prefix="tempest-tasks-proof-") as directory:
    workspace = pathlib.Path(directory)
    sessions = workspace / "sessions"
    script = workspace / "faux.json"
    script.write_text('[{"text":"delivered"}]')
    config = workspace / "extensions.json"
    config.write_text(json.dumps([{"id": "tasks", "argv": [sys.executable, str(repository / "tempest/extension/testdata/tasks.py")], "capabilities": ["tasks", "memo"]}]))
    peers = []
    service = None
    service_thread = None
    try:
        peer = Peer(workspace, config, script, sessions)
        peers.append(peer)
        session = peer.call("session/new", {"cwd": str(workspace)})["sessionId"]
        wake = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=2)
        peer.call("_tempest/durable/create_task", {"sessionId": session, "task": {"id": "reminder", "definition": "tasks/flow", "input": {"wakeAt": wake.isoformat()}, "background": True}})
        peer.call("_tempest/durable/resume_pending", {"sessionId": session})
        assert peer.call("_tempest/durable/get_task", {"sessionId": session, "taskId": "reminder"})["status"] == "sleeping"
        alarm = peer.call("_tempest/durable/next_wake", {"sessionId": session})
        assert alarm["present"]
        peer.crash()
        peer = Peer(workspace, config, script, sessions)
        peers.append(peer)
        peer.call("session/load", {"sessionId": session, "cwd": str(workspace)})
        assert peer.call("_tempest/durable/get_task", {"sessionId": session, "taskId": "reminder"})["status"] == "sleeping"
        wait_until(lambda: datetime.datetime.now(datetime.timezone.utc) >= wake)
        peer.call("_tempest/durable/resume_pending", {"sessionId": session})
        wait_until(lambda: peer.call("_tempest/durable/get_submission", {"sessionId": session, "requestId": "task:reminder:delivery"})["state"] == "completed")
        peer.crash()
        peer = Peer(workspace, config, script, sessions)
        peers.append(peer)
        peer.call("session/load", {"sessionId": session, "cwd": str(workspace)})
        peer.call("_tempest/durable/resume_pending", {"sessionId": session})
        receipt = peer.call("_tempest/durable/get_submission", {"sessionId": session, "requestId": "task:reminder:delivery"})
        assert receipt["state"] == "completed" and receipt["answer"] == "delivered"
        print("PASS persisted sleep restart: sleeping on reopen, one stable delivery receipt, deadline exposed", flush=True)

        effects = {}
        effect_attempts = []
        refund_entered = workspace / "refund-entered"
        refund_release = workspace / "refund-release"
        class Checkout(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers["Content-Length"]))
                key = self.headers["Idempotency-Key"]
                effect_attempts.append(key)
                if key in effects:
                    assert effects[key] == body
                effects[key] = body
                if self.path == "/refund" and json.loads(body)["task"] == "abort-crash-child":
                    refund_entered.write_text("accepted")
                    wait_until(refund_release.exists)
                self.send_response(200)
                self.end_headers()
                try:
                    self.wfile.write(b'{}')
                except BrokenPipeError:
                    # The original harness was killed after effect acceptance.
                    self.close_connection = True
            def log_message(self, format, *args):
                return
        service = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Checkout)
        service_thread = threading.Thread(target=service.serve_forever)
        service_thread.start()
        entered = workspace / "entered"
        release = workspace / "release"
        url = "http://127.0.0.1:" + str(service.server_port)
        peer.call("_tempest/durable/create_task", {"sessionId": session, "task": {"id": "checkout", "definition": "tasks/flow", "input": {"mode": "parent", "service": url, "entered": str(entered), "release": str(release)}, "background": True}})
        peer.send("_tempest/durable/resume_pending", {"sessionId": session})
        wait_until(entered.exists)
        assert entered.read_text() == "1"
        peer.crash()
        release.write_text("resume")
        peer = Peer(workspace, config, script, sessions)
        peers.append(peer)
        peer.call("session/load", {"sessionId": session, "cwd": str(workspace)})
        assert peer.call("_tempest/durable/get_task", {"sessionId": session, "taskId": "checkout-child"})["status"] == "running"
        peer.call("_tempest/durable/resume_pending", {"sessionId": session})
        parent = peer.call("_tempest/durable/get_task", {"sessionId": session, "taskId": "checkout"})
        child = peer.call("_tempest/durable/get_task", {"sessionId": session, "taskId": "checkout-child"})
        assert parent["status"] == child["status"] == "completed"
        assert child["attempt"] == 2 and len(effect_attempts) == 2 and len(effects) == 1
        print("PASS owned phase restart: child attempt 2, two effect requests, one deduplicated charge, parent completed after child", flush=True)

        entered_crash = workspace / "crasher-entered"
        release_crash = workspace / "crasher-release"
        peer.call("_tempest/durable/create_task", {"sessionId": session, "task": {"id": "crasher", "definition": "tasks/flow", "input": {"mode": "child", "service": url, "entered": str(entered_crash), "release": str(release_crash)}, "background": True}})
        for attempt in (1, 2, 3):
            peer.send("_tempest/durable/resume_pending", {"sessionId": session})
            wait_until(lambda: entered_crash.exists() and entered_crash.read_text() == str(attempt))
            peer.crash()
            peer = Peer(workspace, config, script, sessions)
            peers.append(peer)
            peer.call("session/load", {"sessionId": session, "cwd": str(workspace)})
        peer.call("_tempest/durable/resume_pending", {"sessionId": session})
        parked = peer.call("_tempest/durable/get_task", {"sessionId": session, "taskId": "crasher"})
        assert parked["status"] == "parked" and parked["crashes"] == 3
        release_crash.write_text("retry")
        peer.call("_tempest/durable/retry_task", {"sessionId": session, "taskId": "crasher"})
        peer.call("_tempest/durable/resume_pending", {"sessionId": session})
        retried = peer.call("_tempest/durable/get_task", {"sessionId": session, "taskId": "crasher"})
        assert retried["status"] == "completed" and retried["attempt"] == 4
        assert effect_attempts.count("crasher:run:charge") == 4
        print("PASS crash policy: three interrupted phases park, explicit retry completes attempt 4 with one external effect key", flush=True)

        abort_entered = workspace / "abort-entered"
        abort_release = workspace / "abort-release"
        peer.call("_tempest/durable/create_task", {"sessionId": session, "task": {"id": "abort-crash", "definition": "tasks/flow", "input": {"mode": "parent", "service": url, "entered": str(abort_entered), "release": str(abort_release)}, "background": True}})
        peer.send("_tempest/durable/resume_pending", {"sessionId": session})
        wait_until(abort_entered.exists)
        peer.send("_tempest/durable/cancel_task", {"sessionId": session, "taskId": "abort-crash"})
        wait_until(refund_entered.exists)
        peer.crash()
        refund_release.write_text("resume cleanup")
        peer = Peer(workspace, config, script, sessions)
        peers.append(peer)
        peer.call("session/load", {"sessionId": session, "cwd": str(workspace)})
        peer.call("_tempest/durable/resume_pending", {"sessionId": session})
        parent = peer.call("_tempest/durable/get_task", {"sessionId": session, "taskId": "abort-crash"})
        child = peer.call("_tempest/durable/get_task", {"sessionId": session, "taskId": "abort-crash-child"})
        assert parent["status"] == child["status"] == "aborted"
        assert effect_attempts.count("abort-crash-child:abort:refund") == 2
        assert effect_attempts[-1] == "abort-crash:abort:refund"
        print("PASS abort restart: child refund retried under one key, child terminal before parent compensation", flush=True)
    finally:
        for peer in peers:
            peer.close()
        if service is not None:
            service.shutdown()
            service.server_close()
            service_thread.join()
