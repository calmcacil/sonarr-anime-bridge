.PHONY: check format-check module-check lint test build vulnerability workflows docs docker-build

GOLANGCI_LINT_VERSION := v2.13.1
ACTIONLINT_VERSION := v1.7.12
GOVULNCHECK_VERSION := v1.6.0
PROJECT_GO_TOOLCHAIN := go$(shell awk '$$1 == "go" { print $$2; exit }' go.mod)
TOOL_RUN := GOTOOLCHAIN=$(PROJECT_GO_TOOLCHAIN) go run
HOST_ARCH := $(shell go env GOARCH)

check: format-check module-check lint test build vulnerability workflows docs docker-build

format-check:
	@test -z "$$(gofmt -l .)" || { echo "Run 'gofmt -w .' on:"; gofmt -l .; exit 1; }

module-check:
	go mod tidy -diff
	go mod verify
	go vet ./...

lint:
	$(TOOL_RUN) github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m ./...

test:
	go test -race -count=1 ./...

build:
	@for arch in amd64 arm64; do \
		GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
			-ldflags="-s -w -X main.version=local" \
			-o "/tmp/sonarr-anime-bridge-linux-$$arch" ./cmd/server || exit 1; \
	done

vulnerability:
	$(TOOL_RUN) golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

workflows:
	@command -v shellcheck >/dev/null || { echo "shellcheck is required for workflow validation"; exit 1; }
	$(TOOL_RUN) github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

docs:
	python3 scripts/check-doc-links.py

docker-build:
	docker build --platform=linux/$(HOST_ARCH) --build-arg TARGETOS=linux --build-arg TARGETARCH=$(HOST_ARCH) --build-arg VERSION=local -t sonarr-anime-bridge:check-host .
	docker run --rm --platform=linux/$(HOST_ARCH) sonarr-anime-bridge:check-host --version | grep -Fx local
	docker build --platform=linux/amd64 --build-arg TARGETOS=linux --build-arg TARGETARCH=amd64 --build-arg VERSION=local -t sonarr-anime-bridge:check-amd64 .
	docker build --platform=linux/arm64 --build-arg TARGETOS=linux --build-arg TARGETARCH=arm64 --build-arg VERSION=local -t sonarr-anime-bridge:check-arm64 .
