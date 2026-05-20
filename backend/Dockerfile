# Multi-stage build for backend service
FROM golang:1.25-alpine AS builder
WORKDIR /app

# Copy dependency configuration files from workspace root
COPY go.mod go.sum ./
RUN go mod download

# Copy shared package files and backend code
COPY pkg/ ./pkg
COPY backend/ ./backend

# Build the backend binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /smart-router-backend ./backend/main.go

# Minimal runtime image
FROM alpine:3.19
RUN apk --no-cache add ca-certificates
WORKDIR /
COPY --from=builder /smart-router-backend /smart-router-backend
EXPOSE 8080
ENTRYPOINT ["/smart-router-backend"]
