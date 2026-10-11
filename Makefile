.PHONY: check check-images regression format-check module-check lint test build vulnerability workflows docs docker-build

GOLANGCI_LINT_VERSION := v2.14.0
ACTIONLINT_VERSION := v1.7.12
GOVULNCHECK_VERSION := v1.8.0
PROJECT_GO_TOOLCHAIN := go$(shell awk '$$1 == "go" { print $$2; exit }' go.mod)
TOOL_RUN := GOTOOLCHAIN=$(PROJECT_GO_TOOLCHAIN) go run
HOST_ARCH := $(shell go env GOARCH)

check: format-check module-check lint test build vulnerability workflows docs

check-images: check docker-build

regression:
	./testdata/native-regression.sh

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
			-buildvcs=false -ldflags="-s -w -X main.version=local" \
			-o "/tmp/sonarr-anime-bridge-linux-$$arch" ./cmd/server || exit 1; \
	done

vulnerability:
	$(TOOL_RUN) golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

workflows:
	@command -v shellcheck >/dev/null || { echo "shellcheck is required for workflow validation"; exit 1; }
	# v1.7.12 predates GitHub's queue property; regression tests validate the one usage.
	$(TOOL_RUN) github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) -ignore '^unexpected key "queue" for "concurrency" section\.'
	shellcheck testdata/runtime-smoke.sh testdata/rootless-smoke.sh scripts/setup-rootless-ci.sh testdata/release-registry.sh testdata/native-regression.sh testdata/container-build-audit.sh
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s scripts -p 'test_*.py' -v

docs:
	python3 scripts/check-doc-links.py

docker-build:
	@case '$(HOST_ARCH)' in amd64|arm64) ;; *) echo 'Unsupported host architecture: $(HOST_ARCH)' >&2; exit 1 ;; esac
	@for arch in amd64 arm64; do \
		DOCKER_BUILDKIT=1 docker build --platform=linux/$$arch \
			--build-arg TARGETOS=linux --build-arg TARGETARCH=$$arch --build-arg VERSION=local \
			-t sonarr-anime-bridge:check-$$arch . || exit 1; \
	done
	@version=$$(docker run --rm --platform=linux/$(HOST_ARCH) sonarr-anime-bridge:check-$(HOST_ARCH) --version) || exit 1; \
		test "$$version" = local || { echo "Unexpected native version: $$version" >&2; exit 1; }
	sh testdata/container-build-audit.sh
