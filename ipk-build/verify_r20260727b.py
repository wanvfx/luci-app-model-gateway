#!/usr/bin/env python3
import tarfile, gzip, io, sys

IPK = "ipk-build/luci-app-model-gateway_1.5.2-r20260727b_all.ipk"
ok = True
def fail(msg):
    global ok
    ok = False
    print("  [FAIL]", msg)
def info(msg):
    print("  [OK]  ", msg)

# 1) outer magic = gzip tar
with open(IPK, "rb") as f:
    magic = f.read(2)
if magic == b"\x1f\x8b":
    info("outer is gzip (magic 1f 8b)")
else:
    fail("outer magic not gzip: " + magic.hex())

with tarfile.open(IPK, "r:gz") as tf:
    names = tf.getnames()
    for req in ["./debian-binary", "./control.tar.gz", "./data.tar.gz"]:
        if req in names:
            info("outer member " + req + " present")
        else:
            fail("outer member " + req + " MISSING")
    ctrl = tf.extractfile("./control.tar.gz").read()
    data = tf.extractfile("./data.tar.gz").read()

# 2) control fields
with tarfile.open(fileobj=io.BytesIO(ctrl), mode="r:gz") as ctf:
    cnames = ctf.getnames()
    cfile = ctf.extractfile("./control").read().decode("utf-8")
    for fld in ["Package: luci-app-model-gateway", "Version: 1.5.2-r20260727b",
                "Architecture: all", "Source: luci-app-model-gateway", "Maintainer: Zoyaya",
                "Depends: luci-base"]:
        if fld in cfile:
            info("control has: " + fld)
        else:
            fail("control missing: " + fld)
    for ex in ["./postinst", "./prerm"]:
        if ex in cnames:
            m = ctf.getmember(ex)
            if m.mode & 0o755 == 0o755:
                info(ex + " mode 755")
            else:
                fail(ex + " mode not 755: " + oct(m.mode))
        else:
            fail(ex + " missing")

# 3) data members: ./ prefix + permissions + forbidden files
with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as dtf:
    dnames = dtf.getnames()
    for n in dnames:
        # 根目录条目可能是 '.' 或 './'（mkipk 写入 './'，tarfile 读回为 '.'），属正常
        if n in (".", "./"):
            continue
        if not n.startswith("./"):
            fail("member without ./ prefix: " + n)
    for ex in ["./usr/bin/model-gatewayd", "./usr/bin/model-gatewayd-arm64",
               "./etc/init.d/model-gateway", "./usr/libexec/istorec/model-gateway.sh"]:
        if ex in dnames:
            m = dtf.getmember(ex)
            if m.mode & 0o755 == 0o755:
                info(ex + " mode 755")
            else:
                fail(ex + " mode not 755: " + oct(m.mode))
        else:
            fail("critical member missing: " + ex)
    for forb in ["./usr/bin/model-gatewayd.exe", "Dockerfile", ".dockerignore"]:
        if any(n == forb or n.startswith(forb) for n in dnames):
            fail("forbidden item present: " + forb)
    info("no .exe / Dockerfile / .dockerignore")
    for need in ["./usr/share/model-gateway/htdocs/index.html",
                 "./usr/lib/opkg/meta/model-gateway.json",
                 "./usr/share/rpcd/acl.d/luci-app-model-gateway.json"]:
        if need in dnames:
            info("present: " + need)
        else:
            fail("MISSING: " + need)
    h = dtf.extractfile("./usr/share/model-gateway/htdocs/index.html").read().decode("utf-8", "ignore")
    if "1.5.2" in h:
        info("htdocs contains version 1.5.2")
    else:
        fail("htdocs missing version 1.5.2")
    if "./usr/bin/model-gatewayd" in dnames and "./usr/bin/model-gatewayd-arm64" in dnames:
        info("both amd64+arm64 binaries packaged")

print("\nRESULT:", "ALL PASS" if ok else "HAS FAILURES")
sys.exit(0 if ok else 1)
