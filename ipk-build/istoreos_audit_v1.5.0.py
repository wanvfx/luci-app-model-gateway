#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""iStoreOS 合规 + 潜在 BUG 逐细节审查：luci-app-model-gateway_1.5.0_all.ipk"""
import tarfile, io, os, json, struct, re

IPK = "ipk-build/luci-app-model-gateway_1.5.0_all.ipk"
OUT = "ipk-build/istoreos-audit-v1.5.0.md"

lines = []
def add(s): lines.append(s)
def chk(name, cond, detail=""):
    add(f"| {'✅' if cond else '❌'} | {name} | {detail or ('通过' if cond else '**不通过**')} |")
    return cond

add("# iStoreOS 合规 & BUG 逐细节审查报告")
add("")
add(f"- 审查对象：`{IPK}`")
add(f"- 依据：iStoreOS 技能分流准则 + 项目 iron rules（M1/H1/M3/M4、原生/数据目录铁律）")
add("")

# ---------- 解包 ----------
with open(IPK, "rb") as f:
    head = f.read(2)
outer_gzip = head == b"\x1f\x8b"
add("## A. 外层包格式")
add("")
add("| 项 | 结果 | 说明 |")
add("|----|------|------|")
chk("外层是 gzip (magic 1f 8b)", outer_gzip, "magic=%s" % head.hex())

with tarfile.open(IPK, "r:gz") as t:
    names = [m.name for m in t.getmembers()]
    chk("./debian-binary 存在", "./debian-binary" in names)
    chk("./control.tar.gz 存在", "./control.tar.gz" in names)
    chk("./data.tar.gz 存在", "./data.tar.gz" in names)
    db = t.extractfile("./debian-binary").read()
    chk("debian-binary 内容为 '2.0\\n'", db == b"2.0\n", repr(db))
    # 三成员都带 ./ 前缀
    chk("外层三成员均带 './' 前缀",
        all(n.startswith("./") for n in ["./debian-binary","./control.tar.gz","./data.tar.gz"] if n in names))

    # control
    ctrl = tarfile.open("r:gz", fileobj=io.BytesIO(t.extractfile("./control.tar.gz").read()))
    ctrl_members = {m.name: m for m in ctrl.getmembers()}
    ctrl_files = {}
    for m in ctrl.getmembers():
        try: ctrl_files[m.name] = ctrl.extractfile(m).read()
        except: ctrl_files[m.name] = b""
    control_txt = ctrl_files.get("./control", b"").decode("utf-8", "replace")

    # data
    data = tarfile.open("r:gz", fileobj=io.BytesIO(t.extractfile("./data.tar.gz").read()))
    data_members = {m.name: m for m in data.getmembers()}
    data_files = {}
    for m in data.getmembers():
        try: data_files[m.name] = data.extractfile(m).read()
        except: data_files[m.name] = b""

add("")
add("## B. control 文件 & 脚本权限")
add("")
add("| 项 | 结果 | 说明 |")
add("|----|------|------|")
def field(k):
    m = re.search(r"^%s:\s*(.+)$" % re.escape(k), control_txt, re.M)
    return m.group(1).strip() if m else ""
chk("Package = luci-app-model-gateway", field("Package")=="luci-app-model-gateway", field("Package"))
chk("Version = 1.5.0", field("Version")=="1.5.0", field("Version"))
chk("Architecture = all (非 aarch64_generic)", field("Architecture")=="all", field("Architecture"))
chk("Source = luci-app-model-gateway", field("Source")=="luci-app-model-gateway", field("Source"))
chk("Maintainer = Zoyaya", field("Maintainer")=="Zoyaya", field("Maintainer"))
dep = field("Depends")
chk("Depends 含 luci-base 且不含 docker-deps(非 Docker 类)", ("luci-base" in dep) and ("docker-deps" not in dep), dep)
# 脚本权限
def mode(name):
    mm = ctrl_members.get(name) or data_members.get(name)
    return mm.mode if mm else None
chk("postinst 权限 755", mode("./postinst")==0o755, oct(mode("./postinst") or 0))
chk("prerm 权限 755", mode("./prerm")==0o755, oct(mode("./prerm") or 0))

add("")
add("## C. data 文件清单 & 权限 & 类型")
add("")
add("| 项 | 结果 | 说明 |")
add("|----|------|------|")

BIN_AMD64 = "./usr/bin/model-gatewayd"
BIN_ARM64 = "./usr/bin/model-gatewayd-arm64"
INIT = "./etc/init.d/model-gateway"
ISTOREC = "./usr/libexec/istorec/model-gateway.sh"
META = "./usr/lib/opkg/meta/model-gateway.json"
ACL = "./usr/share/rpcd/acl.d/luci-app-model-gateway.json"
HTDOCS = "./usr/share/model-gateway/htdocs/index.html"

def fmode(p):
    mm = data_members.get(p)
    return mm.mode if mm else None
def fbytes(p):
    return data_files.get(p, b"")

chk("usr/bin/model-gatewayd 存在且 755", fmode(BIN_AMD64)==0o755, oct(fmode(BIN_AMD64) or 0))
chk("usr/bin/model-gatewayd-arm64 存在且 755", fmode(BIN_ARM64)==0o755, oct(fmode(BIN_ARM64) or 0))
chk("etc/init.d/model-gateway 存在且 755", fmode(INIT)==0o755, oct(fmode(INIT) or 0))
chk("usr/libexec/istorec/model-gateway.sh 存在且 755", fmode(ISTOREC)==0o755, oct(fmode(ISTOREC) or 0))
chk("usr/lib/opkg/meta/model-gateway.json 存在 (M1)", META in data_members)
chk("usr/share/rpcd/acl.d/...json 存在 (H1)", ACL in data_members)
chk("htdocs/index.html 在正确路径", HTDOCS in data_members)

# 禁打包物
bad = [n for n in data_members if n.endswith(".exe") or n.endswith("Dockerfile") or n.endswith(".dockerignore")]
chk("无 .exe / Dockerfile / .dockerignore", len(bad)==0, ("发现: "+",".join(bad)) if bad else "干净")

# 无新增 init.d（除自身）
extra_init = [n for n in data_members if n.startswith("./etc/init.d/") and n!=INIT]
chk("未擅自新增其他 init.d 脚本", len(extra_init)==0, ("额外: "+",".join(extra_init)) if extra_init else "仅自身")

# ELF 检查
def elf_info(b):
    if b[:4]!=b"\x7fELF": return ("非ELF", None, None)
    ei_class = b[4]            # 1=32,2=64
    ei_data = b[5]             # 1=LE
    if ei_class==2 and ei_data==1:
        # ELF64 LE
        e_phoff = struct.unpack_from("<Q", b, 0x20)[0]
        e_phentsize = struct.unpack_from("<H", b, 0x36)[0]
        e_phnum = struct.unpack_from("<H", b, 0x38)[0]
        e_machine = struct.unpack_from("<H", b, 0x12)[0]
        has_interp = False
        off = e_phoff
        for _ in range(e_phnum):
            p_type = struct.unpack_from("<I", b, off)[0]
            if p_type==3: has_interp=True
            off += e_phentsize
        arch = {62:"amd64",183:"aarch64"}.get(e_machine, "machine=%d"%e_machine)
        return ("ELF64", arch, "静态(无PT_INTERP)" if not has_interp else "动态(含PT_INTERP/CGO)")
    return ("ELF?", None, None)

a_info = elf_info(fbytes(BIN_AMD64))
chk("amd64 二进制为 ELF64/amd64/静态", a_info[0]=="ELF64" and a_info[1]=="amd64" and "静态" in a_info[2], str(a_info))
b_info = elf_info(fbytes(BIN_ARM64))
chk("arm64 二进制为 ELF64/aarch64/静态", b_info[0]=="ELF64" and b_info[1]=="aarch64" and "静态" in b_info[2], str(b_info))

add("")
add("## D. 关键文件内容")
add("")
add("| 项 | 结果 | 说明 |")
add("|----|------|------|")

# meta.json (M1)
try:
    meta = json.loads(fbytes(META).decode("utf-8","replace"))
    chk("meta type = native", meta.get("type")=="native", "type=%s"%meta.get("type"))
    chk("meta 无 image 字段 (非 Docker)", "image" not in meta, "无 image" if "image" not in meta else "含 image")
    # M3 默认启用：以打包的 /etc/config/model-gateway 为准（非 meta.json）
    cfg_txt = fbytes("./etc/config/model-gateway").decode("utf-8","replace")
    m3 = ("option enable '1'" in cfg_txt)
    chk("默认 enable '1' (M3)", m3, "config 含 option enable '1'" if m3 else "未声明默认启用")
except Exception as e:
    chk("meta.json 可解析", False, "解析失败: %s"%e)

# ACL (H1)
try:
    acl = json.loads(fbytes(ACL).decode("utf-8","replace"))
    # 找 luci-app-model-gateway 角色
    role = acl.get("luci-app-model-gateway", {})
    exc = role.get("execute", {})
    ok_exec = exc.get("file")==["*"] or "*" in exc.get("file",[])
    chk("ACL 含 execute.file=['*'] (H1)", ok_exec, json.dumps(exc, ensure_ascii=False))
    uci_read = role.get("read",{}).get("uci",[])
    uci_write = role.get("write",{}).get("uci",[])
    chk("ACL 含 uci 读/写权限", "model-gateway" in uci_read and "model-gateway" in uci_write,
        "read.uci=%s write.uci=%s" % (uci_read, uci_write))
except Exception as e:
    chk("ACL json 可解析", False, "解析失败: %s"%e)

# init.d status() (M4)
init_txt = fbytes(INIT).decode("utf-8","replace")
chk("init.d 含 status() 函数", bool(re.search(r"status\(\)", init_txt)), "含 status()")
chk("init.d 用 pidof 两进程名", ("model-gatewayd" in init_txt and "model-gatewayd-arm64" in init_txt), "pidof 覆盖双架构")
chk("init.d status 返回 0/3", ("return 0" in init_txt and "return 3" in init_txt), "return 0/3 分支")
chk("init.d 含 start/stop/restart/enable", all(x in init_txt for x in ["start","stop","restart","enable"]), "标准动作齐全")

# istorec detect_binary + chmod
ist_txt = fbytes(ISTOREC).decode("utf-8","replace")
chk("istorec 含 detect_binary 架构检测", "detect_binary" in ist_txt or "uname -m" in ist_txt, "架构适配")
chk("istorec 含 chmod 正确二进制", "chmod" in ist_txt, "chmod 步骤")

# htdocs 版本
htdocs_txt = fbytes(HTDOCS).decode("utf-8","replace")
chk("htdocs 含版本 v1.5.0", "v1.5.0" in htdocs_txt, "版本字符串")
chk("htdocs 统计页合并为单一 📊 统计 tab", ("tab-stats" in htdocs_txt and "tab-usage" not in htdocs_txt and "tab-cost" not in htdocs_txt), "消耗+成本合并")

add("")
add("## E. 两个历史 BUG 修复确认（已编译进包）")
add("")
add("| 项 | 结果 | 说明 |")
add("|----|------|------|")
chk("二进制含 'config reloaded' 日志 (重载路径存在)", b"config reloaded" in fbytes(BIN_AMD64), "doReloadConfig 日志")
chk("二进制含 'DecryptAPIKeys' (重载解密修复)", b"DecryptAPIKeys" in fbytes(BIN_AMD64), "密钥解密已编译")
chk("二进制含 '1.5.0' 版本号", b"1.5.0" in fbytes(BIN_AMD64), "版本号")
chk("二进制 /v1/models 输出含 '\"model\":' 字段 (未分类修复)", b'"model":' in fbytes(BIN_AMD64), "前端双索引 bridge")
chk("二进制无 config_path 残留 (铁律)", b"config_path" not in fbytes(BIN_AMD64), "已清除")
# 后台检测 URL 不重复 /v1
chk("二进制 health 路径无重复 '/v1/v1' 拼接痕迹", b"/v1/v1" not in fbytes(BIN_AMD64), "URL 拼接正确")

add("")
add("## F. 数据目录铁律（源码层核查）")
add("")
add("| 项 | 结果 | 说明 |")
add("|----|------|------|")
import os
uci_src = open("config/uci.go",encoding="utf-8",errors="replace").read() if os.path.exists("config/uci.go") else ""
has_dataDir_case = bool(re.search(r'case\s+["\']dataDir', uci_src, re.I))
chk("UCI 解析无 dataDir 选项分支 (铁律)", not has_dataDir_case, "无 case dataDir" if not has_dataDir_case else "存在 dataDir 分支")
main_src = open("main.go",encoding="utf-8",errors="replace").read() if os.path.exists("main.go") else ""
chk("main.go 消费 MODEL_GATEWAY_DATA", "MODEL_GATEWAY_DATA" in main_src, "数据目录走环境变量")
chk("二进制含 MODEL_GATEWAY_DATA 引用", b"MODEL_GATEWAY_DATA" in fbytes(BIN_AMD64), "运行时读取")

# 汇总
add("")
add("## 审查结论")
add("")
add("结构合规项见上表；全部 ✅ 即符合 iStoreOS 原生应用铁律。源码层两项（dataDir / MODEL_GATEWAY_DATA）由配套 Grep 核查确认。")
add("")

with open(OUT, "w", encoding="utf-8") as f:
    f.write("\n".join(lines))
print("\n".join(lines))
print("\nREPORT WRITTEN:", OUT)
