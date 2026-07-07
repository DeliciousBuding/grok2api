FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS TARGETARCH TARGETVARIANT
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w -X main.Version=$(cat VERSION)" -o grok2api .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/grok2api .
COPY config.defaults.toml .
RUN mkdir -p /app/data /app/logs && chown -R 65532:65532 /app/data /app/logs
EXPOSE 8000
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -Y off -qO- "http://127.0.0.1:${SERVER_PORT:-8000}/health" >/dev/null || exit 1
ENTRYPOINT ["/app/grok2api"]
