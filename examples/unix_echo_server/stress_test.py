#!/usr/bin/env python3
"""
Stress tests for unix_echo_server.tin
Run with: python3 stress_test.py
Server binary path: /tmp/unix_echo_server (override via TIN_UNIX_SERVER env)
"""

import socket, threading, time, os, subprocess, sys

SOCK_PATH = "/tmp/tin_unix_echo.sock"
PASS, FAIL = [], []


def conn(timeout=4):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    s.connect(SOCK_PATH)
    return s


def send_recv(s, msg):
    s.sendall(msg if isinstance(msg, bytes) else msg.encode())
    return s.recv(65536).decode(errors="replace")


def record(name, ok, detail=""):
    if ok:
        PASS.append(name)
        print(f"  PASS  {name}")
    else:
        FAIL.append(name)
        print(f"  FAIL  {name}" + (f": {detail}" if detail else ""))


# ── tests ─────────────────────────────────────────────────────────────────────

def test_basic_echo():
    s = conn()
    r = send_recv(s, "hello world\n")
    s.sendall(b"quit\n")
    s.close()
    record("basic echo", "ECHO: hello world" in r, repr(r))


def test_quit_closes():
    s = conn()
    s.sendall(b"quit\n")
    try:
        s.settimeout(0.5)
        leftover = s.recv(256)
        s.close()
        record("quit closes connection", leftover == b"", repr(leftover))
    except Exception:
        s.close()
        record("quit closes connection", True)


def test_multiple_lines():
    s = conn()
    for i in range(10):
        s.sendall(f"line {i}\n".encode())
        r = s.recv(4096).decode()
        if f"ECHO: line {i}" not in r:
            s.close()
            record("multiple lines", False, f"i={i} got={repr(r)}")
            return
    s.sendall(b"quit\n")
    s.close()
    record("multiple lines (10)", True)


def test_concurrent_connections():
    errors = []

    def client(i):
        try:
            s = conn()
            r = send_recv(s, f"conn-{i}\n")
            s.sendall(b"quit\n")
            s.close()
            if f"ECHO: conn-{i}" not in r:
                errors.append(f"{i}: {repr(r)}")
        except Exception as e:
            errors.append(f"{i}: {e}")

    threads = [threading.Thread(target=client, args=(i,)) for i in range(50)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    record("concurrent connections (50)", not errors, str(errors[:3]))


def test_large_payload():
    s = conn(timeout=8)
    payload = "x" * (64 * 1024 - 1) + "\n"
    s.sendall(payload.encode())
    chunks = []
    while True:
        try:
            c = s.recv(65536)
            if not c:
                break
            chunks.append(c.decode(errors="replace"))
            if "ECHO:" in "".join(chunks):
                break
        except Exception:
            break
    r = "".join(chunks)
    s.sendall(b"quit\n")
    s.close()
    record("64 KB line", "ECHO: " in r and len(r) > 1000, f"len={len(r)}")


def test_rapid_connections():
    errors = []
    for i in range(100):
        try:
            s = conn(timeout=2)
            r = send_recv(s, f"rapid-{i}\n")
            s.sendall(b"quit\n")
            s.close()
            if f"ECHO: rapid-{i}" not in r:
                errors.append(f"{i}: {repr(r)}")
        except Exception as e:
            errors.append(f"{i}: {e}")
    record("rapid connect/disconnect (100)", not errors, str(errors[:3]))


def test_throughput():
    s = conn(timeout=10)
    n = 500
    ok = True
    for i in range(n):
        s.sendall(f"tp-{i}\n".encode())
        r = s.recv(4096).decode()
        if f"ECHO: tp-{i}" not in r:
            ok = False
            record("throughput", False, f"i={i} got={repr(r)}")
            s.close()
            return
    s.sendall(b"quit\n")
    s.close()
    record(f"throughput ({n} round-trips)", ok)


# ── main ──────────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    bin_path = os.environ.get("TIN_UNIX_SERVER", "/tmp/unix_echo_server")

    if os.path.exists(SOCK_PATH):
        os.unlink(SOCK_PATH)

    server = subprocess.Popen(
        [bin_path],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )

    for _ in range(30):
        if os.path.exists(SOCK_PATH):
            break
        time.sleep(0.1)
    time.sleep(0.05)

    try:
        test_basic_echo()
        test_quit_closes()
        test_multiple_lines()
        test_concurrent_connections()
        test_large_payload()
        test_rapid_connections()
        test_throughput()
    finally:
        server.terminate()
        server.wait()
        if os.path.exists(SOCK_PATH):
            os.unlink(SOCK_PATH)

    print(f"\nPASSED: {len(PASS)}  FAILED: {len(FAIL)}")
    if FAIL:
        sys.exit(1)
