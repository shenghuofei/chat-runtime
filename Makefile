# chat-runtime Makefile
# 构建、测试、交叉编译与发布打包。

# ---- 变量 ----
BINARY      := chat-runtime
MODULE      := github.com/chat-runtime/chat-runtime
DIST        := dist

# 版本信息：优先取 git tag，其次取短 commit；构建时间为 UTC。
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# 通过 -ldflags 注入版本与构建时间到 main 包变量。
LDFLAGS     := -s -w \
	-X 'main.Version=$(VERSION)' \
	-X 'main.BuildTime=$(BUILD_TIME)'

# 交叉编译目标平台列表（GOOS/GOARCH）。
PLATFORMS   := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.DEFAULT_GOAL := build
.PHONY: build build-linux build-all test lint clean release install tidy help

## build: 编译当前平台二进制到 ./$(BINARY)
build:
	@echo ">> 构建 $(BINARY) $(VERSION)"
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

## build-linux: 编译 Linux amd64 静态二进制到 dist/
build-linux:
	@echo ">> 构建 $(BINARY) $(VERSION) [linux/amd64]"
	@mkdir -p $(DIST)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY)-linux-amd64 .
	@echo "   -> $(DIST)/$(BINARY)-linux-amd64"

## build-all: 交叉编译所有目标平台到 dist/
build-all: clean
	@echo ">> 交叉编译所有平台"
	@mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=$(DIST)/$(BINARY)-$${os}-$${arch}; \
		echo "   -> $${out}"; \
		GOOS=$${os} GOARCH=$${arch} CGO_ENABLED=0 \
			go build -trimpath -ldflags "$(LDFLAGS)" -o $${out} . || exit 1; \
	done

## test: 运行全部测试（含竞态检测）
test:
	go test ./... -v -race

## lint: 运行 go vet 静态检查
lint:
	go vet ./...

## clean: 清理构建产物
clean:
	rm -rf $(DIST)

## release: 交叉编译并打包为 tar.gz
release: build-all
	@echo ">> 打包发布产物"
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		bin=$(BINARY)-$${os}-$${arch}; \
		echo "   -> $(DIST)/$${bin}.tar.gz"; \
		tar -czf $(DIST)/$${bin}.tar.gz -C $(DIST) $${bin} || exit 1; \
	done
	@echo ">> 生成校验和"
	@cd $(DIST) && shasum -a 256 *.tar.gz > checksums.txt 2>/dev/null \
		|| (cd $(DIST) && sha256sum *.tar.gz > checksums.txt)

## install: go install 到 GOPATH/bin
install:
	go install -trimpath -ldflags "$(LDFLAGS)" .

## tidy: 整理依赖
tidy:
	go mod tidy

## help: 显示可用目标
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
