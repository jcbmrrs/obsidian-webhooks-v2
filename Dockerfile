# Build stage
FROM golang:1.24.4-alpine AS builder

# Install ca-certificates for scratch image
RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build with static linking for scratch image
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" \
    -o webhooks-server \
    main.go

# Final stage
FROM scratch

# Copy ca-certificates from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy binary
COPY --from=builder /app/webhooks-server /webhooks-server

# Copy templates
COPY --from=builder /app/templates /templates

# Expose port
EXPOSE 8080

# Run binary
ENTRYPOINT ["/webhooks-server"]