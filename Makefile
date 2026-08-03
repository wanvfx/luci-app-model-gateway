.PHONY: build clean test install verify

BIN_DIR := dist
SRC_DIR := .

# A14：从 git tag 自动注入 IPK_VERSION（如 v1.8.3 -> 1.8.3）
GIT_TAG := $(shell git describe --tags --always 2>/dev/null || echo "dev")
IPK_VERSION := $(patsubst v%,%,$(GIT_TAG))

build:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o $(BIN_DIR)/model-gatewayd ./$(SRC_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o $(BIN_DIR)/model-gatewayd-arm64 ./$(SRC_DIR)
	@echo "build done: $(BIN_DIR)/model-gatewayd (amd64), $(BIN_DIR)/model-gatewayd-arm64 (arm64) [tag=$(GIT_TAG) ipk_version=$(IPK_VERSION)]"

clean:
	rm -rf $(BIN_DIR)

test:
	go test ./...

vet:
	go vet ./...

# A14：构建门禁（build + vet + test + 基本校验）
verify: build vet test verify-version
	@echo "verify passed: tag=$(GIT_TAG) ipk_version=$(IPK_VERSION)"

# M2 门禁：版本号三处（api/admin.go appVersion、htdocs 徽章、meta.json）必须一致。
# 用法：make verify-version 或作为 verify 的子步骤自动执行。
verify-version:
	@v=$$(grep -oE 'const appVersion = "[0-9]+\.[0-9]+\.[0-9]+"' api/admin.go | grep -oE '[0-9]+\.[0-9]+\.[0-9]+'); \
	if [ -z "$$v" ]; then echo "❌ 无法从 api/admin.go 解析 appVersion"; exit 1; fi; \
	echo "期望版本: $$v"; \
	grep -q "v$$v</span>" htdocs/index.html || { echo "❌ htdocs 徽章版本未同步为 v$$v"; exit 1; }; \
	grep -q "当前版本：<b>v$$v</b>" htdocs/index.html || { echo "❌ htdocs 当前版本未同步为 v$$v"; exit 1; }; \
	grep -q "\"version\": \"$$v\"" ipk-build/ipk-build-root/usr/lib/opkg/meta/model-gateway.json || { echo "❌ meta.json version 未同步为 $$v"; exit 1; }; \
	grep -qE "^Version: $$v-r" ipk-build/control/control || { echo "❌ control 文件版本未同步为 $$v-r<rev>"; exit 1; }; \
	echo "✅ 版本号三处一致 ($$v)"

# C1：将源码 htdocs / 数据文件同步到打包根，避免“打包根与源码不同步”导致免 Key 等功能缺失。
# 单一事实来源 = htdocs/、etc/、luci/ 与仓库根数据文件；ipk-build/ipk-build-root/ 仅是打包暂存区。
# P2-6：luci/（Lua 控制器/CBI/view/i18n + istorec + ACL + opkg meta）已确立为真源并纳入同步与校验。
BUILD_ROOT := ipk-build/ipk-build-root
HTDOCS_DST := $(BUILD_ROOT)/usr/share/model-gateway/htdocs
LUCI_DST  := $(BUILD_ROOT)/usr

# 需从 luci/ 真源同步到打包根的文件（相对 BUILD_ROOT 的 usr/ 前缀）
LUCI_FILES := \
	usr/lib/lua/luci/controller/model-gateway.lua \
	usr/lib/lua/luci/i18n/zh-cn/model-gateway.po \
	usr/lib/lua/luci/model/cbi/model-gateway.lua \
	usr/lib/lua/luci/view/model-gateway/status.htm \
	usr/lib/opkg/meta/model-gateway.json \
	usr/libexec/istorec/model-gateway.sh \
	usr/share/rpcd/acl.d/luci-app-model-gateway.json

sync-build:
	@test -f htdocs/index.html || { echo "❌ 缺少 htdocs/index.html"; exit 1; }
	@mkdir -p $(HTDOCS_DST)
	@cp -f htdocs/index.html $(HTDOCS_DST)/index.html
	@cp -f providers_catalog.json $(BUILD_ROOT)/usr/share/model-gateway/providers_catalog.json 2>/dev/null || true
	@echo "✅ 已同步 htdocs -> $(HTDOCS_DST)"
	@test -f etc/init.d/model-gateway || { echo "❌ 缺少 etc/init.d/model-gateway"; exit 1; }
	@mkdir -p $(BUILD_ROOT)/etc/init.d $(BUILD_ROOT)/etc/config
	@cp -f etc/init.d/model-gateway $(BUILD_ROOT)/etc/init.d/model-gateway
	@chmod 0755 $(BUILD_ROOT)/etc/init.d/model-gateway
	@cp -f etc/config/model-gateway $(BUILD_ROOT)/etc/config/model-gateway
	@echo "✅ 已同步 etc/init.d + etc/config -> $(BUILD_ROOT)/etc"
	# P2-6：同步 luci/ 真源（Lua/istorec/ACL/opkg meta）到打包根
	@for f in $(LUCI_FILES); do \
		test -f luci/$$f || { echo "❌ luci/$$f 缺失"; exit 1; }; \
		mkdir -p $(LUCI_DST)/$$(dirname $$f); \
		cp -f luci/$$f $(LUCI_DST)/$$f; \
	done
	@chmod 0755 $(LUCI_DST)/usr/libexec/istorec/model-gateway.sh
	@echo "✅ 已同步 luci/ 真源（Lua/istorec/ACL/opkg meta）-> $(BUILD_ROOT)/usr"

# C1 断言：打包前确保源码与打包根 htdocs 关键标记一致（防免 Key 功能漏打）。
# P2-6：对 luci/ 真源各文件做 cmp 一致性断言，防 Lua/istorec/ACL/meta 与打包根漂移。
verify-sync:
	@grep -q "anonymous_api_key" htdocs/index.html || { echo "❌ 源码 htdocs 缺少 anonymous_api_key"; exit 1; }
	@grep -q "anonymous_api_key" $(HTDOCS_DST)/index.html || { echo "❌ 打包根 htdocs 与源码不同步（anonymous_api_key 缺失），请先 make sync-build"; exit 1; }
	@cmp -s etc/init.d/model-gateway $(BUILD_ROOT)/etc/init.d/model-gateway || { echo "❌ init.d 与打包根不同步，请先 make sync-build"; exit 1; }
	@cmp -s etc/config/model-gateway $(BUILD_ROOT)/etc/config/model-gateway || { echo "❌ etc/config 与打包根不同步，请先 make sync-build"; exit 1; }
	@grep -q "reload_service()" $(BUILD_ROOT)/etc/init.d/model-gateway || { echo "❌ 打包根 init.d 缺少 reload_service（P1-4）"; exit 1; }
	@for f in $(LUCI_FILES); do \
		cmp -s luci/$$f $(BUILD_ROOT)/$$f || { echo "❌ luci/$$f 与打包根不同步，请先 make sync-build"; exit 1; }; \
	done
	@grep -q "shell_quote" luci/usr/lib/lua/luci/model/cbi/model-gateway.lua || { echo "❌ CBI 缺少 P1-1 注入防护（shell_quote）"; exit 1; }
	@grep -q 'uci -q get model-gateway.settings.port' luci/usr/libexec/istorec/model-gateway.sh || { echo "❌ istorec 路径未用具名段"; exit 1; }
	@echo "✅ 打包同步校验通过"

# 一键打包：同步 -> 校验 -> 调 mkipk.py（IPK_VERSION 可由环境变量覆盖）
package: sync-build verify-sync
	python3 ipk-build/mkipk.py

# L1：gofmt 格式化全部 Go 文件
fmt:
	gofmt -w .

# L1 门禁：存在未格式化文件则失败
fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "❌ 以下文件未 gofmt 格式化:"; gofmt -l .; exit 1; }

install: build
	install -m 0755 $(BIN_DIR)/model-gatewayd /usr/bin/model-gatewayd
	install -d /etc/config
	install -m 0644 etc/config/model-gateway /etc/config/model-gateway
	install -d /etc/init.d
	install -m 0755 etc/init.d/model-gateway /etc/init.d/model-gateway
	/etc/init.d/model-gateway disable
	@echo "installed, run: /etc/init.d/model-gateway enable && /etc/init.d/model-gateway start"
