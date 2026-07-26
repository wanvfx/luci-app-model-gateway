# AI 模型网关（Model Gateway）

一个 OpenAI 兼容的 AI 模型网关，把 NVIDIA、商汤 SenseNova、魔搭 ModelScope、Gemini 等多个平台的免费 LLM 额度聚合成统一接口，支持自动故障转移与基于质量 / 延迟的加权路由。iStoreOS / OpenWrt 原生应用，装进软路由 7×24 小时常驻。

## 功能特性

- **多提供商聚合**：把多个上游 LLM 提供商聚合为一个 OpenAI 兼容接口，客户端只填一个地址和一个 Key
- **自动故障转移 + 熔断**：单个渠道限流 / 故障 / 超时自动换下一个，对话不中断
- **路由组**：内置 `256k`（长文本 / 代码库）、`1m`（超大上下文）等路由组，按可用率 + 延迟加权择优
- **一键配置**：填入各家 API Key 即可自动拉取模型并生成路由组，适合新手
- **实时监控**：每个模型的健康状态、可用率、延迟、探针次数一目了然，支持手动立即检测
- **消耗统计**：按今日 / 7 天 / 30 天统计请求数与 Token 消耗，按「供应商 × 模型」展示明细
- **可选外部存储**：配置文件可迁移到外接硬盘，重装 / 升级不丢配置
- **系统公告**：启动时及公告内容变更后自动弹出（markdown 渲染）

## 安装

方式一：在 iStoreOS 的「iStore 应用商店」搜索 **AI 模型网关面板**（包名 `luci-app-model-gateway`）安装。

方式二：手动安装 ipk

```bash
opkg install luci-app-model-gateway_*.ipk
/etc/init.d/model-gateway enable
/etc/init.d/model-gateway start
```

## 使用

1. 在「已安装」里启用 **AI 模型网关面板**（开机自启打勾）。
2. 浏览器访问 `http://<你的路由器LAN IP>:12211`（服务监听 `12211` 端口，进程名 `model-gatewayd`）。
3. 点「⚡ 一键配置」填入各家 API Key，或到「上游提供商」里手动添加。
4. 在 AI 客户端（ChatBox、OpenWebUI、WorkBuddy 等）填入面板「本地接口信息」卡片里的地址和 Key：

```
本地地址：  http://<路由器LAN IP>:12211/v1
API Key：   sk-local-xxxxxxxxxxxx
```

5. 模型名填路由组名（`256k` / `1m`）即可自动调度最优模型。

## 配置文件

配置通过 UCI 管理，主配置文件为 `/etc/config/model-gateway`。可在面板把「配置文件路径」切到外接硬盘（如 `/mnt/mmc1-4/Configs/model-gateway`）。

## 鉴权

| 接口 | 凭据 |
|------|------|
| `/v1/chat/completions`、`/v1/models` | `Authorization: Bearer <local_api_key>` |
| `/api/*`（管理面板） | `Authorization: Bearer <local_api_key>` |

## 构建

纯 Go 标准库实现，无第三方依赖：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/model-gatewayd-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o dist/model-gatewayd-arm64 .
```

打包为 ipk：

```bash
python3 ipk-build/mkipk.py
```

## 项目地址

https://github.com/wanvfx/luci-app-model-gateway/

## 许可

[CC BY-NC 4.0](https://creativecommons.org/licenses/by-nc/4.0/)
