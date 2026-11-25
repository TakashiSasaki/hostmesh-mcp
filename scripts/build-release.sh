#!/bin/sh
# Run from the repository root. The output directory must be new.
set -eu
if [ "$#" -ne 2 ]; then
    echo "usage: scripts/build-release.sh <new-output-directory> <ed25519-private-key.pem>" >&2
    exit 2
fi
out=$1
key=$2
version=$(cat cmd/hostmesh-mcp/VERSION)
mkdir -- "$out"
for arch in amd64 arm64; do
    name="hostmesh-mcp-$version-linux-$arch"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$out/$name" ./cmd/hostmesh-mcp
done
python3 - "$out" "$version" <<'PY'
import hashlib
import json
from pathlib import Path
import sys

directory = Path(sys.argv[1])
version = sys.argv[2]
artifacts = {}
for arch in ("amd64", "arm64"):
    platform = "linux-" + arch
    binary = directory / f"hostmesh-mcp-{version}-{platform}"
    artifacts[platform] = {
        "url": binary.name,
        "size": binary.stat().st_size,
        "sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
    }
manifest = {"product": "host-status-mcp", "version": version, "artifacts": artifacts}
(directory / "release.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
PY
openssl pkeyutl -sign -rawin -inkey "$key" -in "$out/release.json" -out "$out/release.json.sig"
