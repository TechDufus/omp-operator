# e2e runtime image: the in-pod driver plus test/fakeomp installed as `omp`.
# The e2e suite uses it as the agent image so sessions run without a model.
#
# Build from the repository root:
#   docker build -f images/e2e/Dockerfile -t omp-operator-e2e:dev .

ARG BASE_IMAGE=golang:1.27.2
FROM ${BASE_IMAGE} AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/omp-operator-driver ./cmd/driver \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/omp ./test/fakeomp

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/omp-operator-driver /usr/local/bin/omp-operator-driver
COPY --from=builder /out/omp /usr/local/bin/omp
WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/omp-operator-driver"]
