# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Builder. One image builds all three binaries; they share ~all of their code,
# so building them separately would mean compiling the same packages 3x.
# ---------------------------------------------------------------------------
FROM golang:1.23-alpine AS builder

WORKDIR /src

# Dependencies are copied and downloaded before the source, so editing a .go
# file does not invalidate the (slow) module download layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
# CGO is off: the resulting binaries are static, which is what lets the runtime
# stage be a bare alpine image. It also keeps the build reproducible across the
# Windows/Linux/macOS machines this project is meant to run on. When libvips
# lands (opt-in, later phase) it will be behind a build tag and a separate
# stage, precisely so the default path stays CGO-free.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/photo-organizer ./cmd/cli

# ---------------------------------------------------------------------------
# Runtime.
# ---------------------------------------------------------------------------
FROM alpine:3.20 AS runtime

# ca-certificates for any future outbound TLS; tzdata because timestamp
# handling in this project is load-bearing and a container without a timezone
# database silently reports everything as UTC.
RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 photouser && \
    mkdir -p /var/lib/photo-organizer/thumbnails && \
    chown -R photouser:photouser /var/lib/photo-organizer

COPY --from=builder /out/api /app/api
COPY --from=builder /out/worker /app/worker
COPY --from=builder /out/photo-organizer /usr/local/bin/photo-organizer

# Non-root. The photo mount is read-only anyway, but defence in depth is cheap
# here and this container reads a user's entire personal photo library.
USER photouser
WORKDIR /app

EXPOSE 8080
CMD ["/app/api"]
