# buildctl-daemon image: the HTTP build API layered on top of a BuildKit
# image with Nydus support (https://github.com/nydusaccelerator/buildkit).
#
# Build:
#   docker build -f Dockerfile -t ghcr.io/inclusionai/buildkit-service/buildctl-daemon .
#
# Override BASE_IMAGE to layer the daemon on a different BuildKit image:
#   docker build --build-arg BASE_IMAGE=moby/buildkit:latest ...
ARG BASE_IMAGE=ghcr.io/nydusaccelerator/buildkit:v0.31.1-nydus

FROM golang:1.23 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /out/buildctl-daemon ./cmd/buildctl-daemon && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /out/buildctl-batch ./cmd/buildctl-batch && \
    GOBIN=/out CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go install github.com/fullstorydev/grpcurl/cmd/grpcurl@v1.9.3

FROM ${BASE_IMAGE}
RUN if command -v apk >/dev/null 2>&1; then \
        apk add --no-cache curl; \
    elif command -v apt-get >/dev/null 2>&1; then \
        apt-get update && \
        apt-get install -y --no-install-recommends ca-certificates curl && \
        rm -rf /var/lib/apt/lists/*; \
    else \
        echo "BASE_IMAGE must provide apk or apt-get so curl can be installed" >&2; \
        exit 1; \
    fi
COPY --from=builder /out/buildctl-daemon /usr/bin/buildctl-daemon
COPY --from=builder /out/buildctl-batch /usr/bin/buildctl-batch
COPY --from=builder /out/grpcurl /usr/bin/grpcurl
