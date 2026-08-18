# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

RUN apk add --no-cache gcc musl-dev sqlite-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /app/things-index-server ./cmd/things-index-server

# Final stage
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl sqlite-libs && \
    addgroup -S thingsindex && adduser -S -G thingsindex thingsindex

WORKDIR /app
COPY --from=builder /app/things-index-server /usr/local/bin/things-index-server
RUN mkdir -p /var/lib/things-index && chown thingsindex:thingsindex /var/lib/things-index
USER thingsindex

ENV THINGS_INDEX_LISTEN_ADDR="0.0.0.0:8080"
# Binding all interfaces is required for Docker port publishing; the container
# still sits behind Docker's network layer and the bearer-token checks.
ENV THINGS_INDEX_ALLOW_UNSPECIFIED_BIND="1"
ENV THINGS_INDEX_DB_PATH="/var/lib/things-index/queue.sqlite"

VOLUME ["/var/lib/things-index"]
EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -f http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/things-index-server"]
