IMAGE ?= registry.example.com/cilium-custom-node-exporter:0.1.0

.PHONY: build test image push

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/cilium-custom-node-exporter .

# The BPF tests need root (CAP_BPF and CAP_SYS_ADMIN); without it they are skipped.
test:
	go vet ./...
	go test -v ./...

image:
	docker build -t $(IMAGE) .

push: image
	docker push $(IMAGE)
