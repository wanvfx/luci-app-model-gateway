# AI 模型网关（Model Gateway）

一个 OpenAI 兼容的 AI 模型网关，把 NVIDIA、商汤 SenseNova、魔搭 ModelScope、Gemini、DeepSeek 等多个平台的免费 / 付费 LLM 额度聚合成统一接口，支持自动故障转移与基于质量 / 延迟 / 成本的加权路由。iStoreOS / OpenWrt 原生应用，装进软路由 7×24 小时常驻。

## 功能特性

**聚合与路由**
- 多提供商聚合（任意 OpenAI 兼容 `base_url`）+ 统一模型前缀名
- 路由组（默认 `256k` / `1m`）+ 6 种策略：质量（含动态惩罚）/ 优先级 / 最低延迟 / 最低成本 / 轮询 / **内容分类 `classify`** + `auto` 虚拟模型
- 自动故障转移（全候选顺序切换）+ 熔断 / 健康探活（对齐 Python 参考版逐模型真实探测）
- 别名映射、识图（多模态）、🛡️ 严格能力矩阵（按能力过滤候选，避免文不对题）
- 🆓 免 Key 提供者市场实测标注（17 家逐一实测，`keyfree_status` 五态 verified/adapter/rate_limited/needs_key/incompatible + 原因，卡片徽章 + 排序 + 联动过滤）
- 🔌 通用协议适配引擎（声明式 `AdapterSpec` 规格 + 运行时解释执行，零代码扩展非 OpenAI 兼容端点；内置 duckduckgo / theoldllm / felo / text-plain 四套适配器；`adapters.json` 可覆盖/新增）

**省钱与护栏**
- 语义缓存（精确 + 近重复 simhash，非流式直返 / 流式 SSE 回放）
- 预算 / 余额护栏（按日 warn/block）+ 实时预警 banner
- 并发护栏（全局 + 单提供商两级）
- 价格自动同步（后台轮询覆盖参考价，手动可触发）
- 🕵️ PII 正则脱敏（响应自动打码）
- 🔑 虚拟密钥（子密钥）+ 每密钥配额限流（请求数 / Token / 模型白名单 / 启停 / 吊销）

**可观测与运维**
- 统一管理面板（实时监控 / 📊 统计（消耗 + 成本合并）/ 路由配置 / 网关设置 / 调用日志 / 模型参考库）
- 💰 成本 / 用量仪表盘（按渠道 / 模型 / 日估算 USD）
- 🎛️ 模型卡片启用/停用（稳定性页「操作」列，即时热生效，停用的模型行置灰）
- Prometheus `/metrics` 指标、🔁 幂等键（防重复计费）、🔐 API Key AES 加密落盘
- REST 钩子（SSRF 防护 + 可选 HMAC）、一键配置预设、系统公告、检查更新、深浅主题
- 可选外部存储（配置可迁外接硬盘，重装 / 升级不丢）
- 🖼️ 商店图标（iStoreOS 应用商店正常显示）

## 安装

方式一：在 iStoreOS 的「iStore 应用商店」搜索 **AI 模型网关面板**（包名 `luci-app-model-gateway`）安装。

方式二：手动安装 ipk

```bash
opkg install luci-app-model-gateway_*.ipk
/etc/init.d/model-gateway enable
/etc/init.d/model-gateway start
```

> 🌐 本版默认监听所有网卡（`bind_addr 0.0.0.0`），装好即可从局域网任意电脑直接访问 `http://<路由器LAN IP>:12211`，无需额外配置；如需仅本机访问可改 `bind_addr` 为 `127.0.0.1`。

## 使用教程

> 完整图文教程见 [`announcement.md`](announcement.md)（面板「📢 系统公告」内也内置同样内容）。下面是最短上手路径。

### 首次使用标准流程

1. **打开面板**：浏览器访问 `http://<路由器LAN IP>:12211`，首次输入 `admin_key`（自动生成，也在 `/etc/config/model-gateway` 里）。复制顶部「本地访问地址」与「本地访问密钥」备用。
2. **添加提供商**：点「+ 添加提供商」，填名称 / Base URL / API Key，点「测试连接」→「保存并拉取模型」，在弹窗里勾选要暴露的模型。
3. **建路由组（高可用 + 省钱）**：点「🔀 路由配置」→「+ 添加路由」，组名即客户端用的"模型名"，勾选多个模型前缀名，策略选 `quality`（默认）/`cost`（省钱）/`classify`（按内容分流）等，保存。
4. **开防护**：点「⚙️ 网关设置」打开响应缓存、预算护栏、并发护栏、🛡️ 严格能力矩阵，按需加 REST 钩子，保存。
5. **发虚拟密钥（可选）**：点「🔑 虚拟密钥」→「+ 新建密钥」，设每日请求 / Token 配额与允许模型，复制一次性明文 `sk-vk-xxxx` 分发给他人。
6. **客户端接入**：Base URL 填 `http://<路由器LAN IP>:12211/v1`，API Key 填 `sk-local-xxxx`（管理员）或 `sk-vk-xxxx`（虚拟密钥），模型名填路由组名 / `提供商-模型` / 别名。

### 客户端接入速查

| 你要填的项 | 填什么 | 哪里拿 |
|---|---|---|
| Base URL | `http://<路由器LAN IP>:12211/v1` | 面板「实时监控」顶部「本地访问地址」 |
| API Key | 管理员 `sk-local-xxxx` 或 虚拟密钥 `sk-vk-xxxx` | 面板顶部「本地访问密钥」/ 🔑 虚拟密钥（仅显示一次） |
| 模型名 | 路由组名（如 `my-ai`/`smart`）或 `提供商-模型`（如 `deepseek-deepseek-chat`）或别名 | 路由配置 / 管理输出模型 / 别名映射 |

> 更多：11 个分场景示例、进阶技巧、常见问题排错表，见 [`announcement.md`](announcement.md) 的「完整使用教程」。

## 配置文件

配置通过 UCI 管理，主配置文件为 `/etc/config/model-gateway`。可在面板把「配置文件路径」切到外接硬盘（如 `/mnt/mmc1-4/Configs/model-gateway`）。可写数据（缓存 / 虚拟密钥 / 用量）统一落在 `MODEL_GATEWAY_DATA`（外部盘优先，回退 `/var/lib/model-gateway`），不在 UCI 里，重装 / 升级不丢。

## 鉴权

| 接口 | 凭据 |
|------|------|
| `/v1/chat/completions`、`/v1/models` | `Authorization: Bearer <local_api_key 或 虚拟密钥>` |
| `/api/*`（管理面板） | `Authorization: Bearer <local_api_key>` |

## 管理 API

网关暴露 30+ 个 `/api/*` 管理接口（提供商 / 路由 / 巡检 / 虚拟密钥 / 成本仪表盘 / 缓存 / 钩子等），完整参数与返回见 [`API参考.md`](API参考.md)。

## 构建

纯 Go 标准库实现，无第三方依赖：

```bash
make build
# 等价于：
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/model-gatewayd .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o dist/model-gatewayd-arm64 .
```

打包为 ipk（`mkipk.py` 仅打包、不编译；请先把上面两个二进制覆盖到 `ipk-build-root/usr/bin/`）：

```bash
python3 ipk-build/mkipk.py
```

## 最近更新（v1.9.0 · r20260802c）

### 🆓 免 Key 提供者 + 通用协议适配引擎（本版重点）

- 🏷️ **免 Key 市场实测标注**：17 家标称免 Key 的提供者逐一按网关自身逻辑实测，目录新增 `keyfree_status`（verified / adapter / rate_limited / needs_key / incompatible）+ `keyfree_note`；市场卡片直接显示状态徽章与原因，勾「只看免 Key」按状态排序。
- 🔌 **通用协议适配引擎（Phase C）**：声明式 `AdapterSpec` 规格 + 运行时解释执行，**一份规格 = 一家协议**，免写代码即可接入非 OpenAI 兼容端点。支持自定义 chat/models 路径、预检握手取令牌（TTL 缓存）、请求体模板渲染（占位符渲染成合法 JSON 值，防注入）、响应提取（JSON 路径 / 纯文本）、四类流式（sse-json / sse-text / ndjson / none 统一转 OpenAI SSE，`mode=none` 自动降级单块 SSE）。内置 **duckduckgo / theoldllm / felo / text-plain** 四套适配器；数据目录 `adapters.json` 可覆盖/新增，无需发版。
- 🧩 **免 Key 主链路复原（对标 OmniRoute）**：`auth_scheme="none"` 不注入鉴权头 + `NoAuth` 标记 + `AnonymousAPIKey` 兜底 + `injectProviderSpecificHeaders` 扩展点（opencode / pollinations 已接）。
- 💾 **免费模型自动勾选 + 变动检测**：`free_only=true` 时模型管理弹窗自动勾选免费模型；后台定时巡检免费模型变动（受 UCI 控制）。
- 🔒 **密钥框永久保留**：免 Key 提供者不再隐藏 API Key 框（可留空；连接失败提示需鉴权再填），避免「假免 Key 实则需密钥」无从补救。
- 🐞 **目录硬错误修正**：uncloseai 模型 ID 修正（404→可用）、ovhcloud 补齐 12 个真实对话模型、mimocode 补 `/v1` 并改标需密钥、hackclub 改标需密钥；免 Key 家数 17 → **15**。

> v1.6.0–v1.8.3 之间还落地了 Gemini / Claude / OpenAI Responses 原生协议翻译、Webhooks、A2A、Playground、安全套件等，详见 `RELEASE_NOTES_v1.8.*` 与 `REVIEW_REPORT_2026-07-30.md`。

### 🌐 r20260802c · 默认 LAN 访问 + 安全加固（本版重点）

- 🌐 **默认监听所有网卡（bind_addr 0.0.0.0）**：此前默认只听路由器本机（`127.0.0.1`），从局域网电脑访问会被拒（`ERR_CONNECTION_REFUSED`）。**本版起默认 `0.0.0.0`，装好即可从电脑直接 `http://<路由器LAN IP>:12211` 打开面板**，无需再 SSH 改配置。想更保守可在 UCI 把 `bind_addr` 改回 `127.0.0.1`（仅本机访问，需配反代）。
- 🛡️ **第四轮全方位安全加固 14 项（已全修）**：CBI 命令注入防护（Web 配置表单白名单 + shell 转义）、令牌预检 broadcast 风暴修复、用量写盘并发竞态修复、前端 XSS 转义、verify-key 报错转义、UCI 转义互逆（含换行/单引号的配置读写 round-trip 正确）、admin_key 写失败不再静默回滚、别名/钩子原子替换、打包真源门禁（杜绝打包漂移）、istorec 具名段修正、路由评分权重归一（可用率 0.7 + 延迟 0.3）、密钥强随机（`crypto/rand` 失败即报错）、错误日志不再掩盖根因、Gemini 流式首块补 `role:assistant`。让网关在路由内网里更稳、更安全。

### 免 Key 怎么挑（结论）

17 家实测结论：**3 家真免 Key 出流（pollinations / opencode / uncloseai）+ 3 家经内置适配器打通（duckduckgo / theoldllm / felo）+ 6 家上游限流（g4f 系 / ovhcloud）+ 2 家实为需密钥（hackclub / mimocode）+ 3 家架构不兼容（auggie / veoaifree / chipotle）**。推荐 `pollinations + opencode + uncloseai` 配熔断故障转移。完整教程见 [`announcement.md`](announcement.md)「第九 / 十节」。

## 项目地址

https://github.com/wanvfx/luci-app-model-gateway/

## 许可

[CC BY-NC 4.0](https://creativecommons.org/licenses/by-nc/4.0/)
