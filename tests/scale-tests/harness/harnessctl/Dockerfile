# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
#
# Slim in-cluster injector image (inject + reconcile primitives only).
# Build from the NVSentinel repo root so the data-models replace resolves:
#   docker buildx build -f tests/scale-tests/harness/harnessctl/Dockerfile \
#     --platform linux/amd64,linux/arm64 -t IMAGE --push .

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY data-models ./data-models
COPY tests/scale-tests/harness/harnessctl ./tests/scale-tests/harness/harnessctl
WORKDIR /src/tests/scale-tests/harness/harnessctl
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -tags injector -ldflags='-s -w' -o /out/harness-inject .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/harness-inject /usr/local/bin/harness-inject
CMD ["sleep", "infinity"]
