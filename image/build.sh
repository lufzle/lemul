#!/bin/sh
# Builds the workspace sandbox image.
#
# The supervisor is cross-compiled for linux here rather than inside the
# Dockerfile so the image needs no Go toolchain and stays small. Architecture
# defaults to the host's, which is what makes the local Docker driver a faithful
# rehearsal of the ECS one on an arm64 laptop; pass ARCH to cross-build.
set -eu
cd "$(dirname "$0")"

ARCH=${ARCH:-$(uname -m)}
case "$ARCH" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64|amd64)  GOARCH=amd64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

IMAGE=${IMAGE:-lemul-workspace:dev}
CC_VERSION=${CC_VERSION:-2.1.220}

mkdir -p bin
echo "building linux/$GOARCH binaries"
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -o bin/supervisor ../cmd/supervisor
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -o bin/preflight ../cmd/preflight

echo "building $IMAGE (claude-code $CC_VERSION, linux/$GOARCH)"
docker build --platform "linux/$GOARCH" --build-arg "CC_VERSION=$CC_VERSION" -t "$IMAGE" .
echo "built $IMAGE"
