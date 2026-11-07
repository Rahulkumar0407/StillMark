.PHONY: all build dev clean test demo-real

PLUGIN_NAME = stillmark
OUTPUT_DIR = build
OUTPUT = $(OUTPUT_DIR)/$(PLUGIN_NAME).so

all: build

clean:
	@rm -rf $(OUTPUT_DIR)

build: clean
	@mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=1 go build -buildmode=plugin -o $(OUTPUT) .

dev: clean
	@mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=1 go build -buildmode=plugin -o $(OUTPUT) .

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

install:
	@go run installer.go -action=install -bifrost-dir="$(BIFROST_DIR)" $(if $(CONFIG_PATH),-config-path="$(CONFIG_PATH)",) $(if $(filter 1,$(DRY_RUN)),-dry-run,)

uninstall:
	@go run installer.go -action=uninstall -bifrost-dir="$(BIFROST_DIR)" $(if $(CONFIG_PATH),-config-path="$(CONFIG_PATH)",) $(if $(filter 1,$(DRY_RUN)),-dry-run,)

verify:
	@go run installer.go -action=verify -bifrost-dir="$(BIFROST_DIR)"

test-installer:
	go run test_installer.go


demo-real:
	@./demo_real/run.sh
