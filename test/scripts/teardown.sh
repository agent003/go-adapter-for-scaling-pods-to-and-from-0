#!/usr/bin/env bash
# Delete the entire kind cluster used for sanity testing. Certs in
# test/certs/ are kept so the next setup.sh re-uses them; remove them
# manually if you want a fresh CA.

set -euo pipefail

CLUSTER_NAME="ollama-test"

echo "=== Deleting kind cluster $CLUSTER_NAME ==="
kind delete cluster --name "$CLUSTER_NAME"

echo
echo "Done. Generated certs are still in test/certs/ — delete that dir to wipe them."
