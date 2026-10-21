FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/cilium-policy-exporter .

# Runs as root: reading pinned BPF maps needs CAP_BPF/CAP_SYS_ADMIN and
# root-owned bpffs files.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/cilium-policy-exporter /cilium-policy-exporter
ENTRYPOINT ["/cilium-policy-exporter"]
