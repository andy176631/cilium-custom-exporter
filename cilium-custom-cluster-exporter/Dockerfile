ARG BUILDER_IMAGE=golang:1.24-bookworm
ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${BUILDER_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/cilium-custom-cluster-exporter .

# Only talks to the Kubernetes API, so it runs as an unprivileged user.
FROM ${BASE_IMAGE}
COPY --from=build /out/cilium-custom-cluster-exporter /cilium-custom-cluster-exporter
ENTRYPOINT ["/cilium-custom-cluster-exporter"]
