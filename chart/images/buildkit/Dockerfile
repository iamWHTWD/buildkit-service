# BuildKit worker image with CNI plugins, for chart deployments that enable
# buildkitdConfig.worker.oci.networkMode=cni (plugins land in /usr/lib/cni,
# matching the chart's default cniBinaryPath).
#
# BASE_IMAGE is a BuildKit image built from nydusaccelerator/buildkit; the
# buildkit-base.yml workflow builds it from source and then applies this
# overlay. It can also be used standalone against the published base image:
#
#   docker buildx build --platform linux/amd64 \
#     --build-arg BASE_IMAGE=ghcr.io/nydusaccelerator/buildkit:v0.31.1-nydus \
#     -t ghcr.io/<org>/buildkit:v0.31.1-nydus-cni --push chart/images/buildkit
ARG BASE_IMAGE=ghcr.io/nydusaccelerator/buildkit:v0.31.1-nydus

FROM alpine:3.22 AS cni-plugins
ARG CNI_VERSION=v1.1.1
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache curl
RUN mkdir -p /out \
    && curl -fsSL "https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-${TARGETOS:-linux}-${TARGETARCH:-amd64}-${CNI_VERSION}.tgz" \
       | tar -xz -C /out

FROM ${BASE_IMAGE}
COPY --from=cni-plugins /out/ /usr/lib/cni/
