#!/bin/sh
# Builds the three vendor-side images.
#
# Mirrors image/build.sh: the Go binaries are cross-compiled HERE rather than
# inside their Dockerfiles, so those images carry no toolchain. Same reason,
# same trap -- a raw `docker build` ships whatever is already in deploy/bin/,
# and an unchanged image digest is the only tell.
#
# The console is the exception and builds inside its Dockerfile, because a bun
# build needs the same bun that runs it; there is nothing to save by moving it
# out.
#
#   ./build.sh                 build locally as lemul/lemul-*:dev
#   REGISTRY=<acct>.dkr.ecr.<region>.amazonaws.com PUSH=1 ./build.sh
set -eu
cd "$(dirname "$0")"

ARCH=${ARCH:-$(uname -m)}
case "$ARCH" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64|amd64)  GOARCH=amd64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

REGISTRY=${REGISTRY:-lemul}
TAG=${TAG:-dev}
PUSH=${PUSH:-}

mkdir -p bin
echo "building linux/$GOARCH binaries"
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -o bin/controlplane ../cmd/controlplane
CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -o bin/relay ../cmd/relay

for svc in controlplane relay; do
  img="$REGISTRY/lemul-$svc:$TAG"
  echo "building $img (linux/$GOARCH)"
  docker build --platform "linux/$GOARCH" -f "Dockerfile.$svc" -t "$img" .
done

# From the REPOSITORY ROOT, because the console's Dockerfile copies console/ and
# a build context of deploy/ cannot see a sibling directory.
img="$REGISTRY/lemul-console:$TAG"
echo "building $img (linux/$GOARCH)"
docker build --platform "linux/$GOARCH" -f Dockerfile.console -t "$img" ..

if [ -n "$PUSH" ]; then
  for svc in controlplane relay console; do
    echo "pushing $REGISTRY/lemul-$svc:$TAG"
    docker push "$REGISTRY/lemul-$svc:$TAG"
  done
fi
echo "done"
