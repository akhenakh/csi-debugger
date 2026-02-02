# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the driver
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o csi-debugger-driver .

# Final stage - distroless
FROM gcr.io/distroless/static:nonroot

WORKDIR /

# Copy the binary from builder
COPY --from=builder /app/csi-debugger-driver /csi-debugger-driver

# Use nonroot user (65532:65532)
USER 65532:65532

ENTRYPOINT ["/csi-debugger-driver"]
