# Agent runtime image: omp, mise, git and the in-pod driver.
# It is the default agent image (--default-runtime-image) and runs the auth gateway
# and broker of the chart's authGateway component.
#
# Build from the repository root:
#   docker build -f images/runtime/Dockerfile -t omp-operator-runtime:dev .

ARG GO_IMAGE=golang:1.27.2-trixie
# debian:trixie-slim, pinned by its multi-arch index digest
ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f

# Build the in-pod driver
FROM ${GO_IMAGE} AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/omp-operator-driver ./cmd/driver

FROM ${DEBIAN_IMAGE}

ARG OMP_VERSION=v18.12.1
ARG MISE_VERSION=v2026.10.12

LABEL org.opencontainers.image.source="https://github.com/TechDufus/omp-operator" \
      org.opencontainers.image.description="omp-operator agent runtime: omp, mise, git and omp-operator-driver" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git openssh-client jq python3 bash procps \
    && rm -rf /var/lib/apt/lists/*

# omp and mise are downloaded over HTTPS and verified against the checksum files of their releases.
RUN set -eux; \
    tmp="$(mktemp -d)"; cd "${tmp}"; \
    omp_url="https://github.com/can1357/oh-my-pi/releases/download/${OMP_VERSION}"; \
    curl -fsSL -o omp-linux-x64 "${omp_url}/omp-linux-x64"; \
    curl -fsSL -o SHA256SUMS.txt "${omp_url}/SHA256SUMS.txt"; \
    grep ' omp-linux-x64$' SHA256SUMS.txt | sha256sum -c -; \
    install -m 0755 omp-linux-x64 /usr/local/bin/omp; \
    mise_url="https://github.com/jdx/mise/releases/download/${MISE_VERSION}"; \
    curl -fsSL -o "mise-${MISE_VERSION}-linux-x64" "${mise_url}/mise-${MISE_VERSION}-linux-x64"; \
    curl -fsSL -o SHASUMS256.txt "${mise_url}/SHASUMS256.txt"; \
    grep " \./mise-${MISE_VERSION}-linux-x64\$" SHASUMS256.txt | sed 's# \./# #' | sha256sum -c -; \
    install -m 0755 "mise-${MISE_VERSION}-linux-x64" /usr/local/bin/mise; \
    cd /; rm -rf "${tmp}"

COPY --from=builder /out/omp-operator-driver /usr/local/bin/omp-operator-driver

RUN useradd --uid 10001 --user-group --create-home --home-dir /home/agent --shell /bin/bash agent \
    && mkdir -p /workspace /opt/mise \
    && chown agent:agent /workspace /opt/mise

ENV HOME=/home/agent
USER 10001:10001
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/omp-operator-driver"]
