# SPDX-License-Identifier: Apache-2.0

BINARY := tach

# Only a tag overrides the version in main.go: it is recorded as tach_version in
# every summary, so an untagged build keeps the 0.x-dev version, not a bare hash.
VERSION ?= $(shell git describe --tags --dirty 2>/dev/null)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	$(if $(VERSION),-X main.version=$(VERSION:v%=%)) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.PHONY: build build-arm64 test e2e fuzz lint cover clean tidy

build: ## Build the tach binary
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) .

build-arm64: ## Cross-compile a static linux/arm64 binary (g5g)
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-arm64 .

test: ## Run all tests with the race detector
	go test -race ./...

e2e: ## End-to-end checks against testdata/mock-dcgmi
	bash scripts/e2e.sh

fuzz: ## Fuzz the dmon line parser for 60s
	go test -run '^$$' -fuzz 'FuzzParseDmonLine$$' -fuzztime 60s .

lint: ## Run go vet and golangci-lint
	go vet ./...
	golangci-lint run

cover: ## Run tests with coverage and print a summary
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

tidy: ## Tidy go.mod
	go mod tidy

clean: ## Remove build artifacts
	rm -rf bin dist coverage.out
