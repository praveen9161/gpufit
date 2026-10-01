#!/usr/bin/env bash
# Build the gpufit image (agent + controller binaries).
#
#   ./deploy/agent/build.sh                      # host arch, load into the kind cluster
#   ARCH=amd64 SAVE=/tmp/gpufit.tar ./deploy/agent/build.sh
#                                                # for x86 GPU VMs: save a tarball to import with ctr
set -euo pipefail
cd "$(dirname "$0")/../../agent"

TAG="${TAG:-dev}"
IMAGE="gpufit-agent:${TAG}"
CLUSTER="${KIND_CLUSTER_NAME:-dra-example-driver-cluster}"
ARCH="${ARCH:-$(go env GOARCH)}"   # arm64 on Apple Silicon (kind); amd64 for AWS GPU VMs

set -x
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "bin/gpufit-agent-linux-${ARCH}" ./cmd/gpufit-agent
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "bin/gpufit-controller-linux-${ARCH}" ./cmd/gpufit-controller
docker build --platform "linux/${ARCH}" --build-arg "TARGETARCH=${ARCH}" -t "$IMAGE" .
if [[ -n "${SAVE:-}" ]]; then
  docker save "$IMAGE" -o "$SAVE"
else
  kind load docker-image "$IMAGE" --name "$CLUSTER"
fi
