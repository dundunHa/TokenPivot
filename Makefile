# TokenPivot 根命令入口：只组合已有的 go / npm 命令。
# 不安装依赖、不执行迁移、不管理后台进程，也不加载任何 .env 文件。
# 需要 Go 1.26.4（含同一安装的 gofmt）、Node 26、npm 随 Node 26 分发（本机 11.20.0）、make、POSIX shell；
# make check 另需 race 用的本地 C 编译工具链。配置只经进程环境注入。

.DEFAULT_GOAL := help

.PHONY: help dev-go dev-web build-go build-web build format-check check test-integration test-plugin-dsh

help:
	@echo "TokenPivot 可用目标（默认目标，只打印清单，不做其他事）："
	@echo "  make help              打印本清单"
	@echo "  make dev-go            前台运行 Go 服务（go run ./cmd/tokenpivot），不迁移"
	@echo "  make dev-web           前台运行 Vite 开发服务（127.0.0.1:5173）"
	@echo "  make build-go          构建纯后端 bin/tokenpivot-api 与 bin/tokenpivot-migrate（不需要 Node/dist）"
	@echo "  make build-web         类型检查并生成前端产物 web/dist/"
	@echo "  make build             先构建 web/dist，再构建内嵌前端的 bin/tokenpivot 与 bin/tokenpivot-migrate"
	@echo "  make format-check      gofmt 列出未格式化文件即失败，再跑前端 prettier --check"
	@echo "  make check             全套普通检查（不需要 PostgreSQL）"
	@echo "  make test-integration  构建前端后跑 uiembed,integration 测试（需要专用 PostgreSQL 与两个 TEST 变量）"
	@echo "  make test-plugin-dsh   跑 DSH 用量插件测试（node --test，本地模拟服务端）"
	@echo ""
	@echo "Go 命令一律使用 GOTOOLCHAIN=local 与 -mod=readonly；目标之间不自动安装依赖。"

dev-go:
	GOTOOLCHAIN=local go run -mod=readonly ./cmd/tokenpivot

dev-web:
	npm --prefix web run dev

build-go:
	mkdir -p bin
	GOTOOLCHAIN=local go build -mod=readonly -o bin/tokenpivot-api ./cmd/tokenpivot
	GOTOOLCHAIN=local go build -mod=readonly -o bin/tokenpivot-migrate ./cmd/tokenpivot-migrate

build-web:
	npm --prefix web run build

# 顺序 recipe（而非并行 prerequisites）：先 web，再内嵌 Go，make -j 也不会颠倒。
build:
	@echo "==> [1/3] 构建前端产物 web/dist"
	@$(MAKE) --no-print-directory build-web
	@echo "==> [2/3] 构建内嵌前端的服务二进制 bin/tokenpivot"
	mkdir -p bin
	GOTOOLCHAIN=local go build -mod=readonly -tags=uiembed -o bin/tokenpivot ./cmd/tokenpivot
	@echo "==> [3/3] 构建迁移二进制 bin/tokenpivot-migrate"
	GOTOOLCHAIN=local go build -mod=readonly -o bin/tokenpivot-migrate ./cmd/tokenpivot-migrate

format-check:
	@files="$$(gofmt -l cmd internal db web/*.go)" || { echo "gofmt 执行失败，本次 Go 格式检查不可信" >&2; exit 1; }; \
	if [ -n "$$files" ]; then \
		printf '%s\n' "$$files" >&2; \
		echo "以上文件未通过 gofmt 检查；本目标只报告，不改写文件" >&2; \
		exit 1; \
	fi; \
	echo "==> gofmt 检查通过"
	@echo "==> 前端格式检查（prettier --check）"
	npm --prefix web run format:check

# 顺序 recipe：任一阶段非零即整体失败，不跨阶段汇总成功。
# build 已含前端 typecheck，因此不在此重复调用 typecheck。
check:
	@echo "==> [1/9] Go 与前端格式检查（make format-check）"
	@$(MAKE) --no-print-directory format-check
	@echo "==> [2/9] Go vet（普通构建）"
	GOTOOLCHAIN=local go vet -mod=readonly ./...
	@echo "==> [3/9] 前端 lint"
	npm --prefix web run lint
	@echo "==> [4/9] Go race 测试（普通构建）"
	GOTOOLCHAIN=local go test -mod=readonly -race -count=1 ./...
	@echo "==> [5/9] 前端测试"
	npm --prefix web test
	@echo "==> [6/9] 独立 Go 构建（make build-go）"
	@$(MAKE) --no-print-directory build-go
	@echo "==> [7/9] 前端与内嵌 Go 构建（make build）"
	@$(MAKE) --no-print-directory build
	@echo "==> [8/9] uiembed 的 Go race 测试"
	GOTOOLCHAIN=local go test -mod=readonly -tags=uiembed -race -count=1 ./...
	@echo "==> [9/9] uiembed 的 Go vet"
	GOTOOLCHAIN=local go vet -mod=readonly -tags=uiembed ./...

# 显式的数据库写入操作：不属于 check，需要专用 _test 库与两个 TEST 变量。
test-integration:
	@echo "==> [1/2] 构建前端产物 web/dist"
	@$(MAKE) --no-print-directory build-web
	@echo "==> [2/2] uiembed,integration 联合标签测试"
	GOTOOLCHAIN=local go test -mod=readonly -tags=uiembed,integration -count=1 -p 1 -v ./...

# DSH 用量插件（plugins/dsh）：纯 Node 测试，不需要 npm install、PostgreSQL 或 DSH。
test-plugin-dsh:
	cd plugins/dsh && node --test test/
