GO ?= go
export DATABASE_URL
NPM ?= npm
BUILD_DIR ?= build
TOOLS_DIR := $(CURDIR)/.cache/bin
VERSION ?= dev
COMMIT ?= unknown
BUILD_DATE ?= unknown
LDFLAGS := -s -w -X github.com/sagehou/restfleet/internal/buildinfo.Version=$(VERSION) -X github.com/sagehou/restfleet/internal/buildinfo.Commit=$(COMMIT) -X github.com/sagehou/restfleet/internal/buildinfo.Date=$(BUILD_DATE)
STATICCHECK_PACKAGES = $(filter-out github.com/sagehou/restfleet/internal/server/httpapi,$(shell $(GO) list ./...))

.PHONY: all build cross-build tools generate generate-openapi generate-proto fmt lint test web-install web-build migrate clean

all: lint test build

build: web-build
	mkdir -p $(BUILD_DIR)
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/restfleet-server ./cmd/restfleet-server
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/restfleet-gateway ./cmd/restfleet-gateway
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/restfleet-agent ./cmd/restfleet-agent

cross-build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/restfleet-server-linux-amd64 ./cmd/restfleet-server
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/restfleet-server-linux-arm64 ./cmd/restfleet-server
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/restfleet-gateway-linux-amd64 ./cmd/restfleet-gateway
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/restfleet-gateway-linux-arm64 ./cmd/restfleet-gateway
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/restfleet-agent-linux-amd64 ./cmd/restfleet-agent
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/restfleet-agent-linux-arm64 ./cmd/restfleet-agent

tools:
	mkdir -p $(TOOLS_DIR)
	$(GO) -C tools build -o $(TOOLS_DIR)/oapi-codegen github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen
	$(GO) -C tools build -o $(TOOLS_DIR)/buf github.com/bufbuild/buf/cmd/buf
	$(GO) -C tools build -o $(TOOLS_DIR)/staticcheck honnef.co/go/tools/cmd/staticcheck
	$(GO) -C tools build -o $(TOOLS_DIR)/goose github.com/pressly/goose/v3/cmd/goose
	$(GO) -C tools build -o $(TOOLS_DIR)/protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go
	$(GO) -C tools build -o $(TOOLS_DIR)/protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc

generate: tools generate-openapi generate-proto

generate-openapi:
	$(TOOLS_DIR)/oapi-codegen -config api/openapi/oapi-codegen.yaml api/openapi/restfleet-v1.yaml
	cd web && $(NPM) run generate:api

generate-proto:
	PATH="$(TOOLS_DIR):$$PATH" $(TOOLS_DIR)/buf generate api/proto --template api/proto/buf.gen.yaml

fmt:
	$(GO) fmt ./...
	cd web && $(NPM) exec prettier -- --write .

lint: tools
	$(GO) vet ./...
	$(TOOLS_DIR)/staticcheck $(STATICCHECK_PACKAGES)
	$(TOOLS_DIR)/staticcheck -checks=all,-ST1005 ./internal/server/httpapi
	$(TOOLS_DIR)/buf lint api/proto
	cd web && $(NPM) run lint
	cd web && $(NPM) run typecheck

test:
	$(GO) test ./...
	cd web && $(NPM) test

web-install:
	cd web && $(NPM) ci

web-build:
	cd web && $(NPM) run build

migrate: tools
	@GOOSE_DRIVER=postgres GOOSE_DBSTRING="$$DATABASE_URL" GOOSE_MIGRATION_DIR=db/migrations $(TOOLS_DIR)/goose up

clean:
	$(GO) clean
	cd web && $(NPM) exec rimraf -- dist
