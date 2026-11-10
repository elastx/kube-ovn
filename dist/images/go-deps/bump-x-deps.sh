#!/bin/bash
# Rebuild gobgp and kubectl from the base image with golang.org/x/net and
# golang.org/x/text (and grpc for gobgp) raised to versions that fix the
# scanner's CVEs; no release of either tool carries them yet.

set -euo pipefail

: "${KUBECTL_VERSION:?}" "${GOBGP_VERSION:?}" "${X_NET_VERSION:?}" "${X_TEXT_VERSION:?}" "${GRPC_VERSION:?}"

export CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=mod
OUT=/out
mkdir -p "$OUT"

bump() {
    go get "golang.org/x/net@$X_NET_VERSION" "golang.org/x/text@$X_TEXT_VERSION" "$@"
    go mod tidy
    rm -rf vendor
}

mkdir -p /src/gobgp
curl -sSf -L --retry 5 "https://github.com/osrg/gobgp/archive/refs/tags/v${GOBGP_VERSION}.tar.gz" |
    tar -xz --strip-components=1 -C /src/gobgp
cd /src/gobgp
bump "google.golang.org/grpc@$GRPC_VERSION"
go build -trimpath -o "$OUT/gobgp" ./cmd/gobgp

mkdir -p /src/k8s
curl -sSf -L --retry 5 "https://github.com/kubernetes/kubernetes/archive/refs/tags/${KUBECTL_VERSION}.tar.gz" |
    tar -xz --strip-components=1 -C /src/k8s
cd /src/k8s
bump
export KUBE_ROOT=/src/k8s
source hack/lib/util.sh
source hack/lib/logging.sh
source hack/lib/version.sh
export KUBE_GIT_COMMIT=unknown KUBE_GIT_TREE_STATE=clean KUBE_GIT_VERSION="$KUBECTL_VERSION"
KUBE_GIT_MAJOR=$(echo "$KUBECTL_VERSION" | cut -d. -f1 | tr -d v)
KUBE_GIT_MINOR=$(echo "$KUBECTL_VERSION" | cut -d. -f2)
export KUBE_GIT_MAJOR KUBE_GIT_MINOR
ldflags=$(kube::version::ldflags)
go build -trimpath -ldflags="all=$ldflags -s -w" -o "$OUT/kubectl" ./cmd/kubectl
"$OUT/kubectl" version --client | grep -qx "Client Version: $KUBECTL_VERSION" ||
    { echo "kubectl: wrong version string" >&2; exit 1; }

for f in gobgp kubectl; do
    go version -m "$OUT/$f" | grep -E "golang.org/x/(net|text)\s" | grep -qvE "($X_NET_VERSION|$X_TEXT_VERSION)" &&
        { echo "$f: x/net or x/text not at the pinned version" >&2; exit 1; }
    go version -m "$OUT/$f" | grep -E "^\s+dep\s+golang.org/x/(net|text)\s"
done
go version -m "$OUT/gobgp" | grep -E "^\s+dep\s+google.golang.org/grpc\s+$GRPC_VERSION\s"
