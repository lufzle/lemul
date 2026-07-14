#!/bin/sh
# Builds the runner image.
#
# Mirrors build.sh: the binary is cross-compiled here rather than in the
# Dockerfile, so the image carries no Go toolchain. Same reason, same trap --
# a raw `docker build` ships whatever binary is already in bin/, and an
# unchanged image digest is the only tell.
set -eu
cd "$(dirname "$0")"

ARCH=${ARCH:-$(uname -m)}
case "$ARCH" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64|amd64)  GOARCH=amd64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

IMAGE=${IMAGE:-lemul-runner:dev}

mkdir -p bin
echo "building linux/$GOARCH runner"
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -o bin/runner ../cmd/runner

echo "building $IMAGE (linux/$GOARCH)"
docker build --platform "linux/$GOARCH" -f Dockerfile.runner -t "$IMAGE" .
echo "built $IMAGE"
