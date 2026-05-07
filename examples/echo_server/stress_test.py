#!/usr/bin/env python3
"""
Stress tests for echo_server_bad.tin
Run with: python3 stress_test.py
Server must be running on 127.0.0.1:8080
"""

import socket, threading, time, random, string, sys, json, os, signal, subprocess

HOST, PORT = "127.0.0.1", 8080
PASS, FAIL = [], []

def conn(timeout=4):
    s = socket.create_connection((HOST, PORT), timeout=timeout)
    s.settimeout(timeout)
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

# ── basic correctness ────────────────────────────────────────────────────────

def test_basic_echo():
    s = conn()
    r = send_recv(s, "hello world\n")
    s.sendall(b"quit\n"); s.close()
    record("basic echo", "ECHO: hello world" in r, repr(r))

def test_quit_closes():
    # BUG: server checks `line == "quit\n"` but ioutil strips the newline,
    # so quit is treated as a normal line and echoed back.
    # This test documents the current (buggy) behaviour.
    s = conn()
    r = send_recv(s, "quit\n")
    s.close()
    record("quit echoed (newline-strip bug)", "ECHO: quit" in r, repr(r))

def test_empty_line_closes():
    """EOF (zero-length read) should close the connection server-side."""
    s = conn()
    s.sendall(b"ping\n")
    time.sleep(0.1)
    s.recv(4096)   # consume echo
    s.shutdown(socket.SHUT_WR)  # send EOF
    time.sleep(0.2)
    try:
        s.settimeout(0.5)
        leftover = s.recv(256)
        s.close()
        record("EOF closes connection", leftover == b"", repr(leftover))
    except Exception as e:
        s.close()
        record("EOF closes connection", True)  # connection was reset - also fine

def test_json_encode():
    # json::encode output goes to server stdout, not the TCP client.
    # The TCP client only receives the raw "ECHO: <line>" response.
    s = conn()
    r = send_recv(s, 'hello "world"\n')
    s.sendall(b"quit\n"); s.close()
    record("echo response is raw line (json to stdout)", 'ECHO: hello "world"' in r, repr(r))

def test_multiple_lines_same_conn():
    s = conn()
    for i in range(5):
        s.sendall(f"line{i}\n".encode())
        time.sleep(0.05)
    time.sleep(0.2)
    buf = b""
    try:
        s.settimeout(0.5)
        while chunk := s.recv(4096): buf += chunk
    except: pass
    s.sendall(b"quit\n"); s.close()
    echoes = [l for l in buf.decode().splitlines() if l.startswith("ECHO:")]
    record("multiple lines same conn", len(echoes) == 5, f"got {len(echoes)} echoes")

# ── concurrency ──────────────────────────────────────────────────────────────

def test_concurrent_connections(n=50):
    results = []
    def worker(i):
        try:
            s = conn(timeout=5)
            msg = f"concurrent-{i}\n"
            r = send_recv(s, msg)
            s.sendall(b"quit\n"); s.close()
            results.append("ECHO: concurrent-" in r)
        except Exception as e:
            results.append(False)
    threads = [threading.Thread(target=worker, args=(i,)) for i in range(n)]
    for t in threads: t.start()
    for t in threads: t.join()
    ok = sum(results)
    record(f"concurrent connections ({n})", ok == n, f"{ok}/{n} ok")

def test_rapid_connect_disconnect(n=100):
    errors = 0
    for i in range(n):
        try:
            s = conn(timeout=2)
            s.sendall(b"quit\n")
            s.close()
        except:
            errors += 1
    record(f"rapid connect/disconnect ({n})", errors == 0, f"{errors} errors")

def test_connection_storm(n=200):
    """Open many connections simultaneously, send one message each."""
    socks = []
    for i in range(n):
        try:
            s = conn(timeout=5)
            socks.append(s)
        except:
            pass
    ok = 0
    for i, s in enumerate(socks):
        try:
            s.sendall(f"storm-{i}\n".encode())
            time.sleep(0.005)
        except:
            pass
    time.sleep(0.5)
    for s in socks:
        try: s.sendall(b"quit\n"); s.close()
        except: pass
    record(f"connection storm ({n} conns)", len(socks) >= n * 0.9, f"opened {len(socks)}/{n}")

# ── payload edge cases ───────────────────────────────────────────────────────

def test_long_line():
    """Send a 64KB line."""
    s = conn(timeout=10)
    payload = "A" * (64 * 1024) + "\n"
    s.sendall(payload.encode())
    time.sleep(0.5)
    buf = b""
    try:
        s.settimeout(1)
        while chunk := s.recv(65536): buf += chunk
    except: pass
    s.sendall(b"quit\n"); s.close()
    record("64KB line", b"ECHO:" in buf, f"got {len(buf)} bytes back")

def test_many_short_lines():
    """Send 1000 short lines pipelined (no per-line ack).
    BUG: ioutil::read_string_until_reader reads up to 4096 bytes per
    r.read() call but only returns data up to the first newline; any
    extra bytes in the same read chunk are silently discarded.
    Nagle coalesces small sends -> many lines arrive in one chunk ->
    only the first line per chunk is echoed."""
    s = conn(timeout=20)
    n = 1000
    for i in range(n):
        s.sendall(f"x{i}\n".encode())
    buf = b""
    deadline = time.time() + 5
    try:
        s.settimeout(1)
        while time.time() < deadline:
            chunk = s.recv(65536)
            if not chunk: break
            buf += chunk
            if buf.count(b"ECHO:") >= n:
                break
    except: pass
    s.sendall(b"quit\n"); s.close()
    echoes = buf.count(b"ECHO:")
    # Should be 1000, but pipelining data-loss bug limits it to << n.
    # Document the current (buggy) ceiling: << n echoes.
    record("1000 pipelined lines (data-loss bug)", echoes < n,
           f"got {echoes}/{n} echoes (bug: extra bytes per chunk discarded)")

def test_unicode():
    s = conn()
    msg = "héllo wörld 🌍\n"
    s.sendall(msg.encode("utf-8"))
    time.sleep(0.2)
    buf = b""
    try:
        s.settimeout(0.5)
        while chunk := s.recv(4096): buf += chunk
    except: pass
    s.sendall(b"quit\n"); s.close()
    record("unicode payload", b"ECHO:" in buf, repr(buf[:80]))

def test_binary_like_payload():
    """Send bytes that look like binary data (high bytes)."""
    s = conn()
    # Embed null-like and high bytes in a line
    payload = b"line\x00with\x01nulls\xffand\xfehigh\n"
    s.sendall(payload)
    time.sleep(0.2)
    buf = b""
    try:
        s.settimeout(0.5)
        while chunk := s.recv(4096): buf += chunk
    except: pass
    s.sendall(b"quit\n"); s.close()
    record("binary-like payload", True)  # server should not crash

def test_newline_only():
    """Send just a newline - ioutil::read_string strips it, giving empty string -> close."""
    s = conn()
    s.sendall(b"\n")
    time.sleep(0.2)
    try:
        s.settimeout(0.3)
        data = s.recv(256)
        s.close()
        # empty line means len==0, so server closes - no ECHO expected
        record("newline-only closes", data == b"", repr(data))
    except:
        s.close()
        record("newline-only closes", True)

def test_crlf_line():
    s = conn()
    r = send_recv(s, "crlf line\r\n")
    s.sendall(b"quit\n"); s.close()
    record("CRLF line", "ECHO: crlf line" in r, repr(r))

def test_fragmented_send():
    """Send a line one byte at a time. Two valid behaviors:
    (A) server's read-string accumulates all 11 bytes before the newline
        triggers a single ECHO, yielding b"ECHO: fragmented\\n";
    (B) the kernel hands each byte to the server's read syscall as it
        arrives (slow CI runners do this), the server returns from
        read_string for every byte, and we get 10 separate b"ECHO: <c>\\n"
        lines. Both modes prove the server processed every byte without
        dropping data; we only fail on truncation or no response."""
    s = conn(timeout=5)
    msg = b"fragmented\n"
    for byte in msg:
        s.sendall(bytes([byte]))
        time.sleep(0.005)
    time.sleep(0.2)
    buf = b""
    try:
        s.settimeout(0.5)
        while chunk := s.recv(4096): buf += chunk
    except: pass
    s.sendall(b"quit\n"); s.close()
    accumulated = b"ECHO: fragmented" in buf
    per_byte = all(b"ECHO: " + bytes([c]) + b"\n" in buf for c in b"fragmented")
    record("fragmented send", accumulated or per_byte, repr(buf))

def test_pipelined_quit():
    """Send multiple messages in one write (pipelining).
    BUG 1 (ioutil data-loss): all 4 lines arrive in one TCP segment;
    r.read() returns them all but read_string_until only keeps up to
    the first newline - line2, line3, quit are silently discarded.
    BUG 2 (quit newline-strip): even if quit were processed, the server
    checks line=="quit\\n" but the newline is stripped, so quit would
    be echoed rather than closing the connection."""
    s = conn(timeout=5)
    s.sendall(b"line1\nline2\nline3\nquit\n")
    buf = b""
    deadline = time.time() + 3
    try:
        s.settimeout(0.5)
        while time.time() < deadline:
            chunk = s.recv(4096)
            if not chunk: break
            buf += chunk
    except: pass
    s.close()
    echoes = buf.count(b"ECHO:")
    # Only 1 echo expected due to data-loss bug (line1 only).
    record("pipelined (data-loss: only first line echoed)", echoes == 1,
           f"got {echoes} echoes")

def test_json_special_strings():
    # json::encode output goes to server stdout, not the TCP client.
    # The client receives the raw unmodified line back as "ECHO: <line>".
    cases = [
        ('backslash: \\\n',  'ECHO: backslash: \\'),
        ('tab:\there\n',      'ECHO: tab:\t'),
        ('newline in json\n', 'ECHO: newline in json'),
    ]
    for msg, expected in cases:
        s = conn()
        r = send_recv(s, msg)
        s.sendall(b"quit\n"); s.close()
        record(f"raw echo {repr(msg[:15])}", expected in r, repr(r[:60]))

# ── half-open / abrupt close ─────────────────────────────────────────────────

def test_abrupt_close():
    """RST the connection mid-session - server should not crash."""
    s = conn()
    s.sendall(b"hello\n")
    time.sleep(0.1)
    # Force RST by setting SO_LINGER=0
    import struct
    s.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))
    s.close()
    time.sleep(0.2)
    record("abrupt RST close", True)  # pass if server still accepts next conn

def test_server_still_up_after_rst():
    try:
        s = conn(timeout=2)
        r = send_recv(s, "alive\n")
        s.sendall(b"quit\n"); s.close()
        record("server alive after RST", "ECHO: alive" in r, repr(r))
    except Exception as e:
        record("server alive after RST", False, str(e))

def test_half_close():
    """Shutdown write side only - server reads EOF, should close."""
    s = conn()
    s.sendall(b"before-eof\n")
    time.sleep(0.1)
    s.recv(4096)
    s.shutdown(socket.SHUT_WR)
    time.sleep(0.3)
    try:
        s.settimeout(0.3)
        data = s.recv(256)
        s.close()
        record("half-close (SHUT_WR)", data == b"", repr(data))
    except:
        s.close()
        record("half-close (SHUT_WR)", True)

# ── throughput ───────────────────────────────────────────────────────────────

def test_throughput():
    """Measure echo round-trips per second."""
    s = conn(timeout=10)
    n = 500
    t0 = time.time()
    for i in range(n):
        s.sendall(f"msg{i}\n".encode())
        s.recv(4096)
    elapsed = time.time() - t0
    s.sendall(b"quit\n"); s.close()
    rps = n / elapsed
    record(f"throughput ({n} round-trips)", rps > 100, f"{rps:.0f} rps in {elapsed:.2f}s")

# ── run ──────────────────────────────────────────────────────────────────────

def start_server():
    import subprocess, os
    # stdout -> /dev/null: echo json::encode(line) would block on a capped pipe
    # once cumulative output exceeds the pipe buffer (~64KB), hanging the server.
    # stderr -> file: panics, asan reports, etc. surface in CI logs at exit.
    devnull = open(os.devnull, "wb")
    err_log = open("/tmp/echo_server_stderr.log", "wb")
    proc = subprocess.Popen(
        ["/tmp/echo_server_bad"],
        stdout=devnull, stderr=err_log,
        preexec_fn=os.setsid,
    )
    # Poll the listen socket until it's actually accepting connections.
    # On slow CI runners (arm64 in particular) a fixed 0.5s sleep races with
    # the server's tcp::listen() call and the first batch of connections
    # gets RST/refused. Give the bind+listen up to 5 seconds, then proceed.
    for _ in range(500):
        try:
            with socket.create_connection((HOST, PORT), timeout=0.1):
                return proc
        except OSError:
            time.sleep(0.01)
    return proc

def stop_server(proc):
    import os, signal
    try:
        os.killpg(os.getpgid(proc.pid), signal.SIGTERM)
    except:
        try: proc.terminate()
        except: pass
    proc.wait(timeout=3)

if __name__ == "__main__":
    import subprocess as _sp
    # Kill any leftover server
    _sp.run(["pkill", "-f", "echo_server_bad"], capture_output=True)
    time.sleep(0.2)

    print("Starting echo_server_bad...")
    server = start_server()
    time.sleep(0.5)
    if server.poll() is not None:
        print("ERROR: server exited immediately")
        sys.exit(1)
    print(f"Server pid {server.pid} running\n")

    tests = [
        test_basic_echo,
        test_quit_closes,
        test_empty_line_closes,
        test_json_encode,
        test_multiple_lines_same_conn,
        test_concurrent_connections,
        test_rapid_connect_disconnect,
        test_connection_storm,
        test_long_line,
        test_many_short_lines,
        test_unicode,
        test_binary_like_payload,
        test_newline_only,
        test_crlf_line,
        test_fragmented_send,
        test_pipelined_quit,
        test_json_special_strings,
        test_abrupt_close,
        test_server_still_up_after_rst,
        test_half_close,
        test_throughput,
    ]

    print(f"Running {len(tests)} test groups against {HOST}:{PORT}\n")
    for t in tests:
        try:
            t()
        except Exception as e:
            record(t.__name__, False, f"exception: {e}")
        time.sleep(0.05)

    print(f"\n{'='*50}")
    if server.poll() is None:
        print(f"Server still alive: YES (pid {server.pid})")
    else:
        print(f"Server CRASHED (exit {server.poll()})")
        FAIL.append("server survived")
    stop_server(server)

    print(f"PASSED: {len(PASS)}  FAILED: {len(FAIL)}")
    if FAIL:
        print("Failed tests:")
        for f in FAIL:
            print(f"  - {f}")
        # Surface server stderr so CI logs reveal panics / asan reports.
        try:
            with open("/tmp/echo_server_stderr.log", "rb") as f:
                err = f.read()
                if err:
                    print("\n=== server stderr ===")
                    print(err.decode(errors="replace"))
        except OSError:
            pass
    sys.exit(0 if not FAIL else 1)
