.PHONY: all build dev clean test

PLUGIN_NAME = cacheguard
OUTPUT_DIR = build
OUTPUT = $(OUTPUT_DIR)/$(PLUGIN_NAME).so

all: build

clean:
	@rm -rf $(OUTPUT_DIR)

build: clean
	@mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=1 go build -buildmode=plugin -o $(OUTPUT) main.go engine.go

dev: clean
	@mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=1 go build -buildmode=plugin -o $(OUTPUT) main.go engine.go

test:
	go test -v ./...

test-race:
	go test -v -race ./...

bench:
	go run run_benchmark.go

demo:
	go run demo.go

integration:
	go run run_integration.go

