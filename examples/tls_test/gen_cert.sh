#!/bin/bash
# Generate a self-signed certificate for localhost TLS testing.
set -e
openssl req -x509 -newkey rsa:2048 -keyout key.pem -out cert.pem -days 365 -nodes \
  -subj "/CN=localhost" \
  -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" 2>/dev/null
echo "Generated cert.pem and key.pem"
