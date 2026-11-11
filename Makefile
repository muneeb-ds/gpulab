.PHONY: build test demo clean

build:
	mkdir -p bin
	go build -o bin/gpulab-server ./cmd/gpulab-server
	go build -o bin/gpulab-agent ./cmd/gpulab-agent

test:
	go vet ./...
	go test -race ./...

demo: build
	GPULAB_ADMIN_TOKEN=demo ./bin/gpulab-server --demo

clean:
	rm -rf bin gpulab-data
