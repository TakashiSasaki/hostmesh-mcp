#!/bin/sh
# Run from the repository root with two native, static Linux peer binaries.
set -eu
if [ "$#" -ne 2 ]; then
    echo "usage: $0 <previous-peer-binary> <current-supervisor-binary>" >&2
    exit 2
fi
old=$(realpath "$1")
current=$(realpath "$2")
work=$(mktemp -d)
image="host-status-supervise-test:$(basename "$work" | tr '[:upper:]' '[:lower:]')"
cleanup() {
    docker image rm "$image" >/dev/null 2>&1 || :
    rm -rf -- "$work"
}
trap cleanup EXIT
CGO_ENABLED=0 go test -c -o "$work/probe" ./cmd/host-status-mcp
tar -cf - --files-from /dev/null | docker image import - "$image" >/dev/null
docker run --rm --network none --read-only --cap-drop ALL --user 65534:65534 \
    --tmpfs /tmp:rw,exec,nosuid,nodev,mode=1777,size=256m \
    -v "$work/probe:/probe:ro" -v "$old:/old:ro" -v "$current:/current:ro" \
    -e HOST_STATUS_SUPERVISE_TEST=1 -e HOST_STATUS_PREVIOUS_BINARY=/old \
    -e HOST_STATUS_TEST_BINARY=/current "$image" /probe -test.v -test.run='^TestSupervisedUpdate$'
