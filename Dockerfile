# Use official golang image as builder
FROM golang:1.23-alpine AS builder

WORKDIR /opt

# Copy go.mod and main.go
COPY go.mod go.sum ./
COPY main.go .

# Build the application
RUN go build -o server main.go

# Use a minimal alpine image for the final container
FROM alpine:latest

RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    bash \
    nodejs \
    npm

WORKDIR /opt

COPY ./package.json ./package-lock.json ./

RUN npm ci

COPY tsconfig.json vite.config.ts ./
# Copy the binary from builder
COPY --from=builder /opt/server .

# Expose port 8080
EXPOSE 8080

# Run the server
CMD ["./server"]
