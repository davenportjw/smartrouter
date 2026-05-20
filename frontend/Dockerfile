# Multi-stage build for frontend service
FROM golang:1.25-alpine AS builder
WORKDIR /app

# Install Templ binary
RUN go install github.com/a-h/templ/cmd/templ@latest

# Copy dependency configuration files from workspace root
COPY go.mod go.sum ./
RUN go mod download

# Copy shared package files and frontend code
COPY pkg/ ./pkg
COPY frontend/ ./frontend

# Generate Compiled Templ Components
RUN templ generate -path ./pkg/dashboard/templates

# Build the frontend binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /smart-router-frontend ./frontend/main.go

# Minimal runtime image
FROM alpine:3.19
RUN apk --no-cache add ca-certificates
WORKDIR /
COPY --from=builder /smart-router-frontend /smart-router-frontend
COPY docs/ /docs
EXPOSE 8081
ENTRYPOINT ["/smart-router-frontend"]
