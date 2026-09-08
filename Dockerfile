# syntax=docker/dockerfile:1

# Stage 1: Build static binary
FROM golang:1.24-alpine AS builder

WORKDIR /build

# Cache dependency layer
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build static, CGO-free binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /build/runnel ./cmd/runnel

# Stage 2: Minimal runtime image
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 10001 -S runnel && \
    adduser -u 10001 -S runnel -G runnel -h /home/runnel && \
    mkdir -p /data /etc/runnel && \
    ln -s /data /home/runnel/data && \
    chown -R runnel:runnel /data /etc/runnel /home/runnel

COPY --from=builder /build/runnel /usr/local/bin/runnel
COPY --chown=runnel:runnel runnel.example.yaml /etc/runnel/runnel.yaml

USER runnel:runnel
WORKDIR /home/runnel

ENV RUNNEL_HOST="0.0.0.0" \
    RUNNEL_PORT="8090"

VOLUME ["/data"]

EXPOSE 8090

ENTRYPOINT ["runnel"]
CMD ["-config", "/etc/runnel/runnel.yaml"]
