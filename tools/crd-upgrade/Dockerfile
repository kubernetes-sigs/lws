# CRD Upgrader image for lws
# This image contains all CRD files and kubectl to upgrade CRDs during Helm upgrade
FROM alpine:3.20

# Copy CRD files and upgrade script
COPY ./config/crd/bases /lws/crds
COPY ./tools/crd-upgrade/ensure-crds-up-to-date.sh /lws/ensure-crds-up-to-date.sh

# Install necessary tools
RUN apk add --update --no-cache bash curl && \
    chmod +x /lws/ensure-crds-up-to-date.sh

# Install kubectl
ARG KUBECTL_VERSION=v1.31.0
ARG TARGETARCH
RUN ARCH="${TARGETARCH:-amd64}" && \
    curl -LO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl" && \
    curl -LO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl.sha256" && \
    echo "$(cat kubectl.sha256)  kubectl" | sha256sum -c && \
    mv kubectl /usr/local/bin/kubectl && \
    chmod +x /usr/local/bin/kubectl && \
    rm kubectl.sha256

ENTRYPOINT ["bash", "/lws/ensure-crds-up-to-date.sh"]
