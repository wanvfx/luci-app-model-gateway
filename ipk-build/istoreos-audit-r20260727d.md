# iStoreOS 原生应用合规审查 — r20260727d

审查对象：`ipk-build/luci-app-model-gateway_1.0.0-r20260727d_all.ipk`
审查方式：解析外层 gzip tar → 提取 `control.tar.gz` / `data.tar.gz` → 逐条对照 iStoreOS 原生应用铁律（M1/M3/H1/M4）与项目 iron rules。

## 结论：✅ 全部通过，无需修正

| 项 | 规则 | 结果 | 证据 |
|----|------|------|------|
| 外层格式 | gzip tar（魔数 `1f 8b`），非 ar | ✅ | 首字节 `1f8b`；外层 3 成员 `./debian-binary ./control.tar.gz ./data.tar.gz` |
| Architecture | `all`（双二进制自检测） | ✅ | control `Architecture: all` |
| M1 原生识别 | `usr/lib/opkg/meta/model-gateway.json` 进包 | ✅ | 存在；`type:"native"`、`app_name:"model-gateway"`、`arch:["amd64","arm64"]`、`port:12211` |
| H1 ACL | 含 `"execute":{"file":["*"]}` | ✅ | `usr/share/rpcd/acl.d/luci-app-model-gateway.json` 含 `execute.file=["*"]` |
| M4 init.d | 含 `status()` + `pidof`，返回 0/3 | ✅ | `etc/init.d/model-gateway` 755；`status()`+`pidof`+`return 0/3` 均在 |
| M3 默认启用 | 默认 `enable '1'` | ✅ | 随包 `etc/config/model-gateway` 含 `option enable '1'`；init.d 有 enable 兜底守卫 |
| 二进制权限 | `model-gatewayd` / `-arm64` 755 | ✅ | 两者均为 755 |
| istorec 脚本 | `usr/libexec/istorec/model-gateway.sh` 755 | ✅ | 755 |
| 面板路径 | `usr/share/model-gateway/htdocs/index.html` | ✅ | 存在，版本号 `r20260727d` ×3 |
| 铁律·禁 dataDir | UCI 无 `data_dir`/`datadir`/`dataDir` 字段 | ✅ | 随包 UCI 无此字段；可写数据走 `MODEL_GATEWAY_DATA`（init.d 注入） |
| 铁律·禁污染物 | 无 `.exe` / `Dockerfile` / `.dockerignore` | ✅ | data.tar.gz 内均无 |
| 铁律·禁新增 init.d | 仅 1 个标准 init.d | ✅ | 仅 `etc/init.d/model-gateway` |
| 铁律·禁 CGO/WASM/Lua | 单静态二进制 | ✅ | 交叉编译 `CGO_ENABLED=0`，无 Lua/WASM 运行时依赖 |
| control 元数据 | 含 `Source` / `Maintainer: Zoyaya` / `Depends: luci-base` | ✅ | 见 control |
| 安装脚本 | `postinst` / `prerm` 755 | ✅ | 两者 755 |

## 附注
- 根目录 `app-meta-model-gateway` 为 iStore 商店用的源描述文件（含 `installed/upgrade` 运行时字段），其生成的 JSON 已正确打包进 `usr/lib/opkg/meta/model-gateway.json`，符合 M1。
- 第二批 4 项（虚拟密钥+配额、严格能力矩阵、成本仪表盘、内容分类路由）后端 + 前端 SPA 均已落地，本审查仅验证打包合规，未改动代码。
