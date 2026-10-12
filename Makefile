# hanime-dl Makefile
#
# 环境陷阱：本机（以及任何 HOME 下 Go 构建缓存只读的机器）/root/.cache/go-build 不可写，
# 不重定向 GOCACHE 时 go build/vet/test 会直接报 "read-only file system"。
# 这里默认把缓存放到仓库内 .dbg/ 下（已加入 .gitignore）；若外部已导出同名环境变量，
# 因使用 ?= 与 export，外部值优先。详见 README「环境陷阱 / GOCACHE」。
GOCACHE ?= $(CURDIR)/.dbg/gocache
GOLANGCI_LINT_CACHE ?= $(CURDIR)/.dbg/golangci-cache
export GOCACHE
export GOLANGCI_LINT_CACHE

# golangci-lint 未必在 PATH 上（本仓库把它装在 .dbg/gopath/bin）。
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(CURDIR)/.dbg/gopath/bin/golangci-lint)

# 只纳入 git 认得的 Go 文件：已跟踪 + 未跟踪但未被 .gitignore 忽略的新文件。
# 这样自动把 .dbg/（本地构建缓存与 GOPATH，内含 1 万+ 第三方 .go 文件）排除在外。
# 注意：刻意不用 `gofmt -w .`——gofmt 会递归进入 .dbg/ 并改写第三方源码。
GO_FILES := $(shell git ls-files --cached --others --exclude-standard '*.go')

.PHONY: all build vet fmt fmt-check lint test

all: fmt-check vet build test lint

build:
	go build ./...

vet:
	go vet ./...

fmt:
	@test -n "$(GO_FILES)" || { echo "GO_FILES 为空，拒绝执行（gofmt 无文件参数时会读取 stdin）"; exit 1; }
	gofmt -l -w $(GO_FILES)

fmt-check:
	@test -n "$(GO_FILES)" || { echo "GO_FILES 为空，拒绝执行（gofmt 无文件参数时会读取 stdin）"; exit 1; }
	@out="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$out" ]; then \
		echo "以下文件未通过 gofmt，请运行 make fmt："; \
		echo "$$out"; \
		exit 1; \
	fi; \
	echo "gofmt: OK"

lint:
	$(GOLANGCI_LINT) run ./...

test:
	go test -race ./...
