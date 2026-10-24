import ast
import json
import os
import signal
import struct
import sys
import traceback

# Protocol pipes belong to the driver, never exec'd user subprocesses.
for descriptor in (3, 4, 5):
    os.set_inheritable(descriptor, False)

request_pipe = os.fdopen(3, "rb", buffering=0)
response_pipe = os.fdopen(4, "wb", buffering=0)
namespace = {"__name__": "__main__"}


def read_exact(size):
    chunks = bytearray()
    while len(chunks) < size:
        chunk = request_pipe.read(size - len(chunks))
        if not chunk:
            raise EOFError("request pipe closed")
        chunks.extend(chunk)
    return bytes(chunks)


def respond(result):
    payload = json.dumps(result).encode("utf-8")
    response_pipe.write(struct.pack(">I", len(payload)) + payload)


deadline_expired = [False]


def interrupt_deadline(signum, frame):
    deadline_expired[0] = True
    raise KeyboardInterrupt("eval cell timed out")


# SIGINT is enabled only inside a cell, not while reading the next frame.
signal.signal(signal.SIGINT, signal.SIG_IGN)
if os.fork() == 0:
    # A separate interpreter observes parent death even if a C extension holds the GIL.
    for descriptor in (0, 1, 2, 3, 4):
        os.close(descriptor)
    os.read(5, 1)
    os.killpg(os.getpgrp(), signal.SIGKILL)
    os._exit(0)
os.close(5)
# The driver starts isolated; workspace imports become available only to cells.
sys.path.insert(0, os.getcwd())
respond({"event": "ready"})
while True:
    try:
        frame_size = struct.unpack(">I", read_exact(4))[0]
        request = json.loads(read_exact(frame_size))
    except EOFError:
        break
    error = ""
    try:
        deadline_expired[0] = False
        signal.signal(signal.SIGALRM, interrupt_deadline)
        signal.signal(signal.SIGINT, signal.default_int_handler)
        respond({"event": "started"})
        if request["timeout_ms"] > 0:
            signal.setitimer(signal.ITIMER_REAL, request["timeout_ms"] / 1000)
        tree = ast.parse(request["code"], filename="<eval>", mode="exec")
        trailing = tree.body.pop() if tree.body and isinstance(tree.body[-1], ast.Expr) else None
        exec(compile(tree, "<eval>", "exec"), namespace, namespace)
        if trailing is not None:
            value = eval(compile(ast.Expression(trailing.value), "<eval>", "eval"), namespace, namespace)
            if value is not None:
                print(repr(value))
    except BaseException as exception:
        # User code may raise SystemExit or KeyboardInterrupt; neither ends the kernel.
        error = type(exception).__name__
        traceback.print_exc()
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        sys.stdout.flush()
        sys.stderr.flush()
    os.write(1, request["boundary"].encode("ascii"))
    respond({"event": "done", "error": error, "timed_out": deadline_expired[0]})
