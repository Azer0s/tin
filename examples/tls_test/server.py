#!/usr/bin/env python3
# TLS echo server for testing Tin's tls module.
# Usage: python3 server.py [port]
#
# The server accepts one connection, echoes back every line prefixed with
# "ECHO: ", then closes.  For the stress test it handles multiple sequential
# connections until killed.

import ssl
import socket
import sys
import os

port = int(sys.argv[1]) if len(sys.argv) > 1 else 8443
script_dir = os.path.dirname(os.path.abspath(__file__))
cert = os.path.join(script_dir, "cert.pem")
key  = os.path.join(script_dir, "key.pem")

ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(cert, key)

with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind(("127.0.0.1", port))
    sock.listen(128)
    print(f"TLS echo server listening on port {port}", flush=True)

    while True:
        try:
            conn, addr = sock.accept()
        except KeyboardInterrupt:
            break
        with ctx.wrap_socket(conn, server_side=True) as tls:
            data = b""
            while True:
                chunk = tls.recv(4096)
                if not chunk:
                    break
                data += chunk
                while b"\n" in data:
                    line, data = data.split(b"\n", 1)
                    resp = b"ECHO: " + line + b"\n"
                    tls.sendall(resp)
