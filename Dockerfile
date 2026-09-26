# Build the provider binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# Cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer. Retry to ride
# out transient proxy.golang.org HTTP/2 stream errors on large module zips.
RUN success=0; \
    for attempt in 1 2 3 4 5; do \
        if go mod download; then success=1; break; fi; \
        echo "go mod download failed (attempt ${attempt}/5); retrying in 5s..."; \
        sleep 5; \
    done; \
    [ "$success" = 1 ]

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o provider cmd/provider/main.go

# Use distroless as minimal base image to package the provider binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/provider .
USER 65532:65532

ENTRYPOINT ["/provider"]
