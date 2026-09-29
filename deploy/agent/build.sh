#!/usr/bin/env bash
# Build the gpufit-agent image and load it into the kind cluster.
#
#   ./deploy/agent/build.sh            # tag "dev"
#   TAG=v0.1 ./deploy/agent/build.sh
set -euo pipefail
cd "$(dirname "$0")/../../agent"

TAG="${TAG:-dev}"
IMAGE="gpufit-agent:${TAG}"
CLUSTER="${KIND_CLUSTER_NAME:-dra-example-driver-cluster}"
ARCH="$(go env GOARCH)"   # arm64 on Apple Silicon; kind nodes match the host

set -x
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "bin/gpufit-agent-linux-${ARCH}" ./cmd/gpufit-agent
docker build --build-arg "TARGETARCH=${ARCH}" -t "$IMAGE" .
kind load docker-image "$IMAGE" --name "$CLUSTER"
