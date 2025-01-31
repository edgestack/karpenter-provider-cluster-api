#!/usr/bin/env bash

set -ex
set -o pipefail

# push to karpenter-provider-cluster-api with default latest tag
TAG=${TAG:-latest}
REPO=${REPO:-quay.io/edgestack}
PUSH=${PUSH:-}

# support other container tools. e.g. podman
CONTAINER_CLI=${CONTAINER_CLI:-docker}
CONTAINER_BUILDER=${CONTAINER_BUILDER:-"buildx build"}

# If set, just building, no pushing
if [[ -z "${DRY_RUN:-}" ]]; then
  PUSH="--push"
fi

# supported platforms
PLATFORMS=linux/amd64,linux/arm64

# shellcheck disable=SC2086 # inteneded splitting of CONTAINER_BUILDER
${CONTAINER_CLI} ${CONTAINER_BUILDER} \
  --platform ${PLATFORMS} \
  ${PUSH} \
  -f Dockerfile \
  -t "${REPO}"/karpenter-provider-cluster-api:"${TAG}" .
