# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o mbproxy ./cmd/mbproxy

# Test stage (use full golang image for race detector support)
FROM golang:1.24 AS test

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go fmt ./... && go vet ./...
RUN go test -v -race ./...

# Final stage - scratch for minimal image
FROM scratch

COPY --from=builder /app/mbproxy /mbproxy

ENV HEALTH_LISTEN=:8080
EXPOSE 8080

HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/mbproxy", "-health"]

ENTRYPOINT ["/mbproxy"]
