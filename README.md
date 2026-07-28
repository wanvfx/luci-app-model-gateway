# AI 模型网关（Model Gateway）

一个 OpenAI 兼容的 AI 模型网关，把 NVIDIA、商汤 SenseNova、魔搭 ModelScope、Gemini、DeepSeek 等多个平台的免费 / 付费 LLM 额度聚合成统一接口，支持自动故障转移与基于质量 / 延迟 / 成本的加权路由。iStoreOS / OpenWrt 原生应用，装进软路由 7×24 小时常驻。

## 功能特性

**聚合与路由**
- 多提供商聚合（任意 OpenAI 兼容 `base_url`）+ 统一模型前缀名
- 路由组（默认 `256k` / `1m`）+ 6 种策略：质量（含动态惩罚）/ 优先级 / 最低延迟 / 最低成本 / 轮询 / **内容分类 `classify`** + `auto` 虚拟模型
- 自动故障转移（全候选顺序切换）+ 熔断 / 健康探活（对齐 Python 参考版逐模型真实探测）
- 别名映射、识图（多模态）、🛡️ 严格能力矩阵（按能力过滤候选，避免文不对题）

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

## 最近更新（r20260728c）

在 v1.5.3（r20260727c）基础上：

- 🔋 **智能巡检计数封顶 20/20**：旧版每日 12 点静默复查会逐日累加「密集巡检进度」，约 19 天涨到 39，与"约 20 次后休眠"矛盾；现已修正，徽标永久封顶 `20/20`，「立即检测」按钮不受影响。
- 🎛️ **模型卡片启用/停用按钮（新功能）**：稳定性页每行新增「操作」列，启用=绿色、停用=灰色，默认启用；停用的模型行整体置灰，点击即时写入 UCI `disabled_models` 并热生效，无需重启服务。
- 🐞 **修复高危前端 bug**：删除了导致整段 SPA 脚本解析失败的多余 `}`，此前会让所有面板按钮 onclick 全部失效，现已恢复正常。
- 🧩 **成本与用量统计页布局修复**：修正标题行容器漏闭合导致预算卡 / 汇总卡 / 三张表格横向错乱的问题，恢复正常纵向布局。

## 项目地址

https://github.com/wanvfx/luci-app-model-gateway/

## 许可

[CC BY-NC 4.0](https://creativecommons.org/licenses/by-nc/4.0/)
