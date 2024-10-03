# SPDX-FileCopyrightText: 2022-present Intel Corporation
#
# SPDX-License-Identifier: Apache-2.0
#

FROM golang:1.23.1-bookworm AS builder

LABEL maintainer="Aether SD-Core <dev@lists.aetherproject.org>"

WORKDIR $GOPATH/src/upfadapter
COPY . .
RUN make all

FROM alpine:3.20 AS upfadapter

LABEL description="Aether open source 5G Core Network" \
    version="Stage 3"

ARG DEBUG_TOOLS

RUN apk update && apk add --no-cache -U bash

# Install debug tools ~ 50MB (if DEBUG_TOOLS is set to true)
RUN if [ "$DEBUG_TOOLS" = "true" ]; then \
        apk update && apk add --no-cache -U vim strace net-tools curl netcat-openbsd bind-tools; \
        fi

# Set working dir
WORKDIR /aether

# Copy executable and default certs
COPY --from=builder /go/src/upfadapter/bin/* .
