ARG BUILDER_IMAGE=golang:1.25
ARG BASE_IMAGE=gcr.io/distroless/static:nonroot

# Build the manager binary
FROM --platform=${BUILDPLATFORM} ${BUILDER_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH
ARG CGO_ENABLED

WORKDIR /workspace
# cache deps before building so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
COPY . .
# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN make build GO_BUILD_ENV='CGO_ENABLED=${CGO_ENABLED} GOOS=linux GOARCH=${TARGETARCH}'

FROM --platform=${BUILDPLATFORM} ${BASE_IMAGE}
WORKDIR /
COPY --from=builder /workspace/bin/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
