# e2e runtime image: the in-pod driver plus test/fakeomp installed as `omp`, and the
# test/gitserver smart-HTTP git server as `omp-e2e-gitserver`. git is installed because
# cloud thread sessions clone, bundle and push with it.
# The e2e suite uses it as the agent image so sessions run without a model.
#
# Build from the repository root:
#   docker build -f images/e2e/Dockerfile -t omp-operator-e2e:dev .

ARG BASE_IMAGE=golang:1.27.2
# debian:trixie-slim, the same digest as images/runtime/Dockerfile
ARG DEBIAN_IMAGE=debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f

FROM ${BASE_IMAGE} AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/omp-operator-driver ./cmd/driver \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/omp ./test/fakeomp \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/omp-e2e-gitserver ./test/gitserver

FROM ${DEBIAN_IMAGE}

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/omp-operator-driver /usr/local/bin/omp-operator-driver
COPY --from=builder /out/omp /usr/local/bin/omp
COPY --from=builder /out/omp-e2e-gitserver /usr/local/bin/omp-e2e-gitserver

RUN useradd --uid 10001 --user-group --create-home --home-dir /home/agent --shell /bin/bash agent \
    && mkdir -p /workspace /opt/mise \
    && chown agent:agent /workspace /opt/mise

ENV HOME=/home/agent
USER 10001:10001
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/omp-operator-driver"]
