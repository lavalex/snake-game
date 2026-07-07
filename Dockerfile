# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy source code
COPY main.go .

# Initialize go module and download the Prometheus dependencies
RUN go mod init snake-game && go mod tidy

# Build the application as a completely static, stripped binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o snake-game .

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/snake-game .

# Make binary executable
RUN chmod +x /app/snake-game

# Create data directory with proper permissions
RUN mkdir -p /data && chmod -R 777 /data && chmod -R 777 /app

# Expose port
EXPOSE 8080

# Set environment variables
ENV PORT=8080
ENV DATA_DIR=/data

# Run as non-root user for restricted environments (like OpenShift SCCs)
USER 1001

CMD ["/app/snake-game"]