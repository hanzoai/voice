# syntax=docker/dockerfile:1

FROM public.ecr.aws/docker/library/golang:1.26.5-alpine AS build
# go.mod pins the toolchain. The golang base sets GOTOOLCHAIN=local, which turns
# a `go` directive newer than the image into a hard failure rather than a fetch.
ENV GOTOOLCHAIN=auto
WORKDIR /src

# Both dependencies — hanzoai/authz and hanzoai/go-openai-realtime — are public
# and served by the module proxy, so this needs no credential and no git. A
# private-module dance here would be apparatus for a problem this tree does not
# have, and one more thing to fail in a build that cannot explain itself.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

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
