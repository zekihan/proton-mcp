GO ?= go
PROJECT_NAME := proton-mcp
OS_INFO := $(shell $(GO) env GOOS)
ARCH_INFO := $(shell $(GO) env GOARCH)
BINARY := dist/$(PROJECT_NAME)_$(OS_INFO)_$(ARCH_INFO)/$(PROJECT_NAME)
TMP_DIR := ./tmp

.PHONY: help dev build build/local build/all build/docker build/release build/release/all build/release/docker run tidy lint test test/go test/cover audit release release-check new_version

help:
	@echo "Targets: dev build run tidy lint test test/go test/cover audit release release-check new_version"

dev:
	$(GO) run .

build: tidy build/local

build/local:
	goreleaser build --clean --id all --snapshot --single-target

build/all:
	goreleaser build --clean --id all --snapshot

build/docker:
	goreleaser build --clean --id docker --snapshot

build/release: build/release/all

build/release/all:
	goreleaser build --clean --id all --skip=validate

build/release/docker:
	goreleaser build --clean --id docker --skip=validate

run: build
	./$(BINARY)

tidy:
	$(GO) fmt ./...
	$(GO) mod tidy -v

lint: tidy
	golangci-lint fmt
	golangci-lint run --enable-only=nlreturn,tagalign,sloglint --fix
	golangci-lint run

test: test/go

test/go:
	$(GO) test -count=1 -buildvcs -vet=all ./...
	$(GO) test -race -count=1 -buildvcs -vet=all ./...

test/cover:
	mkdir -p $(TMP_DIR)
	$(GO) test -race -coverprofile=$(TMP_DIR)/coverage.out ./...
	$(GO) tool cover -html=$(TMP_DIR)/coverage.out

audit: tidy
	$(GO) mod verify
	$(GO) vet ./...
	golangci-lint run --enable-only=staticcheck
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

release-check:
	goreleaser check

release:
	goreleaser release --clean --skip=publish,announce

new_version:
	./scripts/new_version.sh $(VERSION)
