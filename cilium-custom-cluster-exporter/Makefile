# Override the Dockerfile FROM images, e.g. make image BUILDER_IMAGE=mirror/golang:1.24-bookworm
BUILDER_IMAGE ?=
BASE_IMAGE ?=
IMAGE ?= registry.example.com/cilium-custom-cluster-exporter:0.1.0

.PHONY: build test image push

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/cilium-custom-cluster-exporter .

test:
	go vet ./...
	go test -v ./...

image:
	docker build -t $(IMAGE) \
		$(if $(BUILDER_IMAGE),--build-arg BUILDER_IMAGE=$(BUILDER_IMAGE)) \
		$(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) .

push: image
	docker push $(IMAGE)
