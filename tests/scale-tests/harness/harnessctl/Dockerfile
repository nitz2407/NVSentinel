# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
#
# One image, used both as the operator CLI and as the in-cluster Job image
# (harnessctl inject / harnessctl reconcile).
#
# Build:
#   go mod tidy && CGO_ENABLED=0 go build -o harnessctl .
#   docker build -t "$HARNESS_IMAGE" .        # e.g. myregistry/nvsentinel-harness/harnessctl:v1
#   docker push "$HARNESS_IMAGE"

FROM public.ecr.aws/docker/library/alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY harnessctl ./
ENTRYPOINT ["./harnessctl"]
