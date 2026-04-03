#!/usr/bin/env python3
"""
Stress tests for udp_echo_server.tin
Run with: python3 stress_test.py
Server binary path: /tmp/udp_echo_server (override via TIN_UDP_SERVER env)
"""

import socket, threading, time, os, subprocess, sys

HOST, PORT = "127.0.0.1", 8082
PASS, FAIL = [], []


def mk_sock(timeout=4):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(timeout)
    return s


def send_recv(s, data):
    b = data if isinstance(data, bytes) else data.encode()
    s.sendto(b, (HOST, PORT))
    r, _ = s.recvfrom(65536)
    return r


def record(name, ok, detail=""):
    if ok:
        PASS.append(name)
        print(f"  PASS  {name}")
    else:
        FAIL.append(name)
        print(f"  FAIL  {name}" + (f": {detail}" if detail else ""))


# ── tests ─────────────────────────────────────────────────────────────────────

def test_basic_echo():
    s = mk_sock()
    r = send_recv(s, b"hello world")
    s.close()
    record("basic echo", r == b"hello world", repr(r))


def test_binary_payload():
    s = mk_sock()
    data = bytes(range(256))
    r = send_recv(s, data)
    s.close()
    record("binary payload (256 bytes)", r == data, f"len={len(r)}")


def test_large_datagram():
    s = mk_sock(timeout=8)
    data = b"x" * 60000
    r = send_recv(s, data)
    s.close()
    record("large datagram (60 KB)", r == data, f"len={len(r)}")


def test_multiple_sequential():
    s = mk_sock()
    ok = True
    for i in range(20):
        msg = f"seq-{i}".encode()
        r = send_recv(s, msg)
        if r != msg:
            ok = False
            record("sequential datagrams", False, f"i={i} got={repr(r)}")
            s.close()
            return
    s.close()
    record("sequential datagrams (20)", ok)


def test_empty_datagram_ignored():
    """Server ignores empty datagrams; subsequent message still works."""
    s = mk_sock()
    s.sendto(b"", (HOST, PORT))  # server discards, no reply
    time.sleep(0.05)
    r = send_recv(s, b"after-empty")
    s.close()
    record("empty datagram ignored", r == b"after-empty", repr(r))


def test_concurrent_clients():
    errors = []

    def client(i):
        s = mk_sock()
        try:
            msg = f"client-{i}".encode()
            r = send_recv(s, msg)
            if r != msg:
                errors.append(f"{i}: got {repr(r)}")
        except Exception as e:
            errors.append(f"{i}: {e}")
        finally:
            s.close()

    threads = [threading.Thread(target=client, args=(i,)) for i in range(20)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    record("concurrent clients (20)", not errors, str(errors[:3]))


def test_throughput():
    s = mk_sock(timeout=10)
    n = 200
    ok = True
    for i in range(n):
        msg = f"tp-{i}".encode()
        r = send_recv(s, msg)
        if r != msg:
            ok = False
            break
    s.close()
    record(f"throughput ({n} round-trips)", ok)


# ── main ──────────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    bin_path = os.environ.get("TIN_UDP_SERVER", "/tmp/udp_echo_server")
    server = subprocess.Popen(
        [bin_path],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    time.sleep(0.3)

    try:
        test_basic_echo()
        test_binary_payload()
        test_large_datagram()
        test_multiple_sequential()
        test_empty_datagram_ignored()
        test_concurrent_clients()
        test_throughput()
    finally:
        server.terminate()
        server.wait()

    print(f"\nPASSED: {len(PASS)}  FAILED: {len(FAIL)}")
    if FAIL:
        sys.exit(1)
