# syntax=docker/dockerfile:1

FROM public.ecr.aws/docker/library/golang:1.26.5-alpine AS build
# go.mod pins the toolchain. The golang base sets GOTOOLCHAIN=local, which turns
# a `go` directive newer than the image into a hard failure rather than a fetch.
ENV GOTOOLCHAIN=auto
RUN apk add --no-cache git
WORKDIR /src

# hanzoai/authz is private, so it is fetched over authenticated git rather than
# the public proxy. gh_token is the shared build secret; absent it, this is a
# no-op and the build works for anything already public.
ENV GOPRIVATE=github.com/hanzoai/* \
    GONOSUMDB=github.com/hanzoai/*
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=gh_token \
    if [ -s /run/secrets/gh_token ]; then \
      git config --global url."https://x-access-token:$(cat /run/secrets/gh_token)@github.com/".insteadOf "https://github.com/"; \
    fi && \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags="-w -s" -o voice ./cmd/voice

FROM public.ecr.aws/docker/library/alpine:3.21
LABEL maintainer="https://hanzo.ai/"
# Certificates, because every seam this reaches — speech, the model, IAM's JWKS —
# is over TLS.
RUN apk add --no-cache ca-certificates && update-ca-certificates \
    && adduser -D hanzo -u 1000
USER 1000
WORKDIR /
COPY --from=build /src/voice /voice
EXPOSE 8140
ENTRYPOINT ["/voice"]
