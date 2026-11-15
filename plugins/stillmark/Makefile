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

# install / uninstall / verify use the StillMark CLI (same code path as `npx stillmark`).
# From this repo it runs ../../cli; from a copy inside Bifrost it falls back to npx.
STILLMARK_CLI := $(if $(wildcard ../../cli/bin/stillmark.mjs),node ../../cli/bin/stillmark.mjs,npx -y stillmark)
REQUIRE_BIFROST_DIR = @test -n "$(BIFROST_DIR)" || { echo "Set BIFROST_DIR=/path/to/bifrost (a Bifrost source checkout)"; exit 1; }

install:
	$(REQUIRE_BIFROST_DIR)
	@$(STILLMARK_CLI) install --bifrost "$(BIFROST_DIR)" $(if $(CONFIG_PATH),--config "$(CONFIG_PATH)",) $(if $(filter 1,$(DRY_RUN)),--dry-run,)

uninstall:
	$(REQUIRE_BIFROST_DIR)
	@$(STILLMARK_CLI) uninstall --bifrost "$(BIFROST_DIR)" --clean-artifacts $(if $(CONFIG_PATH),--config "$(CONFIG_PATH)",) $(if $(filter 1,$(DRY_RUN)),--dry-run,)

verify:
	$(REQUIRE_BIFROST_DIR)
	@$(STILLMARK_CLI) verify --bifrost "$(BIFROST_DIR)"

test-installer:
	go run test_installer.go


demo-real:
	@./demo_real/run.sh
