#!/usr/bin/env bash
# Generate a self-signed CA, a multi-SAN server certificate, and a client
# certificate for the kind-cluster sanity test.
#
# Outputs (in this directory):
#   ca.crt, ca.key          — root CA
#   server.crt, server.key  — server cert (multi-SAN: nginx-test, gateway, worker, localhost)
#   client.crt, client.key  — client cert for nginx → gateway-caddy mTLS
#
# All certificates are signed by the same CA so a single ca.crt is enough to
# verify any of them. Idempotent: re-running overwrites the artifacts.

set -euo pipefail

cd "$(dirname "$0")"

DAYS="${DAYS:-365}"
NAMESPACE="${NAMESPACE:-ollama-test}"

# ----- openssl configs (heredoc'd into temp files, cleaned up at end) -----
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cat >"$TMP/ca.cnf" <<EOF
[ req ]
prompt              = no
distinguished_name  = dn
x509_extensions     = v3_ca

[ dn ]
CN = ollama-test-ca
O  = ollama-test

[ v3_ca ]
basicConstraints       = critical, CA:TRUE
keyUsage               = critical, keyCertSign, cRLSign
subjectKeyIdentifier   = hash
EOF

cat >"$TMP/server.cnf" <<EOF
[ req ]
prompt             = no
distinguished_name = dn
req_extensions     = v3_req

[ dn ]
CN = ollama-test-server
O  = ollama-test

[ v3_req ]
basicConstraints   = CA:FALSE
keyUsage           = critical, digitalSignature, keyEncipherment
extendedKeyUsage   = serverAuth
subjectAltName     = @alt

[ alt ]
DNS.1  = nginx-test-svc
DNS.2  = nginx-test-svc.${NAMESPACE}.svc.cluster.local
DNS.3  = ollama-gateway-svc
DNS.4  = ollama-gateway-svc.${NAMESPACE}.svc.cluster.local
DNS.5  = ollama-worker-svc
DNS.6  = ollama-worker-svc.${NAMESPACE}.svc.cluster.local
DNS.7  = localhost
IP.1   = 127.0.0.1
EOF

cat >"$TMP/client.cnf" <<EOF
[ req ]
prompt             = no
distinguished_name = dn
req_extensions     = v3_req

[ dn ]
CN = ollama-test-client
O  = ollama-test

[ v3_req ]
basicConstraints  = CA:FALSE
keyUsage          = critical, digitalSignature
extendedKeyUsage  = clientAuth
EOF

# ----- 1. Root CA -----
echo "==> CA"
openssl genrsa -out ca.key 4096 2>/dev/null
openssl req -x509 -new -nodes -key ca.key -days "$DAYS" \
    -out ca.crt -config "$TMP/ca.cnf"

# ----- 2. Server cert (multi-SAN) -----
echo "==> server cert"
openssl genrsa -out server.key 2048 2>/dev/null
openssl req -new -key server.key -out "$TMP/server.csr" -config "$TMP/server.cnf"
openssl x509 -req -in "$TMP/server.csr" \
    -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out server.crt -days "$DAYS" \
    -extensions v3_req -extfile "$TMP/server.cnf"

# ----- 3. Client cert (for nginx → gateway-caddy mTLS) -----
echo "==> client cert"
openssl genrsa -out client.key 2048 2>/dev/null
openssl req -new -key client.key -out "$TMP/client.csr" -config "$TMP/client.cnf"
openssl x509 -req -in "$TMP/client.csr" \
    -CA ca.crt -CAkey ca.key -CAcreateserial \
    -out client.crt -days "$DAYS" \
    -extensions v3_req -extfile "$TMP/client.cnf"

rm -f ca.srl

chmod 600 ./*.key
chmod 644 ./*.crt

echo
echo "Generated:"
ls -1 ./*.crt ./*.key
