# Multi-stage build for the orchestrator binary.
# Produces a minimal distroless image (~20 MB).

FROM golang:1.25-alpine AS builder

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /orchestrator ./cmd/orchestrator

# Runtime image
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /orchestrator /orchestrator

USER nonroot:nonroot

ENTRYPOINT ["/orchestrator"]
