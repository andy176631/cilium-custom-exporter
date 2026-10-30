ARG BUILDER_IMAGE=golang:1.24-bookworm
ARG BASE_IMAGE=gcr.io/distroless/static-debian12

FROM ${BUILDER_IMAGE} AS build
# Go module proxy for `go mod download`, e.g. https://goproxy.example.com,direct
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/cilium-custom-node-exporter .

# Runs as root: reading pinned BPF maps needs CAP_BPF/CAP_SYS_ADMIN and
# root-owned bpffs files.
FROM ${BASE_IMAGE}
COPY --from=build /out/cilium-custom-node-exporter /cilium-custom-node-exporter
ENTRYPOINT ["/cilium-custom-node-exporter"]
