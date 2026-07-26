#!/usr/bin/env python3
"""Build a standards-compliant ipk for OpenWrt/iStoreOS."""

import os
import tarfile
import io
import gzip
import time

PROJECT_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
BUILD_ROOT = os.path.join(PROJECT_DIR, "ipk-build", "ipk-build-root")
CONTROL_DIR = os.path.join(PROJECT_DIR, "ipk-build", "control")
OUTPUT_DIR = os.path.join(PROJECT_DIR, "ipk-build")

os.makedirs(OUTPUT_DIR, exist_ok=True)

PACKAGE = "luci-app-model-gateway"
VERSION = "1.0.0-r20260726z"
ARCH = "all"
SECTION = "luci"
PRIORITY = "optional"
MAINTAINER = "linkease"
LICENSE = "MIT"
DEPENDS = "luci-base"
DESCRIPTION = "Model Gateway - OpenAI compatible API proxy for iStoreOS/OpenWrt."


def write_control_files():
    os.makedirs(CONTROL_DIR, exist_ok=True)
    with open(os.path.join(CONTROL_DIR, "control"), "w", encoding="utf-8", newline="\n") as f:
        f.write(
            f"Package: {PACKAGE}\n"
            f"Version: {VERSION}\n"
            f"Architecture: {ARCH}\n"
            f"Section: {SECTION}\n"
            f"Priority: {PRIORITY}\n"
            f"Maintainer: {MAINTAINER}\n"
            f"License: {LICENSE}\n"
            f"Source: {PACKAGE}\n"
            f"Depends: {DEPENDS}\n"
            f"Description: {DESCRIPTION}\n"
        )
    with open(os.path.join(CONTROL_DIR, "conffiles"), "w", encoding="utf-8", newline="\n") as f:
        f.write("/etc/config/model-gateway\n")
    with open(os.path.join(CONTROL_DIR, "postinst"), "w", encoding="utf-8", newline="\n") as f:
        f.write(
            "#!/bin/sh\n"
            "# 安装/升级后：尊重 UCI 的 enable 标志，不强制自启。\n"
            "# 未启用（enable!=1）：保持停止、不建开机自启软链，避免“装完就跑”。\n"
            "# 已启用（enable=1，通常是升级时保留的用户配置）：拉起并建软链。\n"
            '[ -z "$IPKG_INSTROOT" ] || exit 0\n'
            'if [ "$(uci -q get model-gateway.settings.enable 2>/dev/null)" = "1" ]; then\n'
            "    /etc/init.d/model-gateway enable >/dev/null 2>&1\n"
            "    /etc/init.d/model-gateway restart >/dev/null 2>&1\n"
            "else\n"
            "    /etc/init.d/model-gateway disable >/dev/null 2>&1\n"
            "    /etc/init.d/model-gateway stop >/dev/null 2>&1\n"
            "fi\n"
            "exit 0\n"
        )
    with open(os.path.join(CONTROL_DIR, "prerm"), "w", encoding="utf-8", newline="\n") as f:
        f.write(
            "#!/bin/sh\n"
            "# 卸载/升级前：先停服务（杀掉旧进程），再取消开机自启软链。\n"
            "# 避免重新安装时旧进程残留导致“装完仍运行中”。\n"
            '[ -z "$IPKG_INSTROOT" ] || exit 0\n'
            "/etc/init.d/model-gateway stop >/dev/null 2>&1\n"
            "/etc/init.d/model-gateway disable >/dev/null 2>&1\n"
            "exit 0\n"
        )
    os.chmod(os.path.join(CONTROL_DIR, "postinst"), 0o755)
    os.chmod(os.path.join(CONTROL_DIR, "prerm"), 0o755)


def make_tar_gz(source_dir):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz", format=tarfile.GNU_FORMAT) as tf:
        # 添加根目录条目 `.`
        root_info = tarfile.TarInfo(name="./")
        root_info.type = tarfile.DIRTYPE
        root_info.mode = 0o755
        root_info.uid = 0
        root_info.gid = 0
        root_info.uname = "root"
        root_info.gname = "root"
        tf.addfile(root_info)

        for root, dirs, files in os.walk(source_dir):
            dirs.sort()
            files.sort()
            for d in dirs:
                dir_path = os.path.join(root, d)
                arcname = "./" + os.path.relpath(dir_path, source_dir).replace(os.sep, "/")
                info = tarfile.TarInfo(name=arcname)
                info.type = tarfile.DIRTYPE
                info.mode = 0o755
                info.uid = 0
                info.gid = 0
                info.uname = "root"
                info.gname = "root"
                tf.addfile(info)
            for f in files:
                file_path = os.path.join(root, f)
                arcname = "./" + os.path.relpath(file_path, source_dir).replace(os.sep, "/")
                info = tarfile.TarInfo(name=arcname)
                info.size = os.path.getsize(file_path)
                # data.tar.gz: 二进制、init 脚本、istorec 生命周期脚本保留执行权限，其余 644
                if arcname in ("./usr/bin/model-gatewayd", "./usr/bin/model-gatewayd-arm64", "./etc/init.d/model-gateway", "./usr/libexec/istorec/model-gateway.sh"):
                    info.mode = 0o755
                # control.tar.gz: postinst/prerm 保留执行权限
                elif source_dir == CONTROL_DIR and arcname in ("./postinst", "./prerm"):
                    info.mode = 0o755
                else:
                    info.mode = 0o644
                info.uid = 0
                info.gid = 0
                info.uname = "root"
                info.gname = "root"
                with open(file_path, "rb") as ff:
                    tf.addfile(info, ff)
    return buf.getvalue()


def build_ipk():
    """Build IPK as gzipped tar (standard OpenWrt format)."""
    print("Building ipk...")
    write_control_files()

    control_data = make_tar_gz(CONTROL_DIR)
    data_data = make_tar_gz(BUILD_ROOT)

    debian_binary = b"2.0\n"

    ipk_path = os.path.join(OUTPUT_DIR, f"{PACKAGE}_{VERSION}_{ARCH}.ipk")
    with tarfile.open(ipk_path, "w:gz", format=tarfile.GNU_FORMAT) as tf:
        for name, data in [
            ("./debian-binary", debian_binary),
            ("./control.tar.gz", control_data),
            ("./data.tar.gz", data_data),
        ]:
            info = tarfile.TarInfo(name=name)
            info.size = len(data)
            info.mode = 0o644
            info.uid = 0
            info.gid = 0
            info.uname = "root"
            info.gname = "root"
            with io.BytesIO(data) as bio:
                tf.addfile(info, bio)
    print(f"Built: {ipk_path}")
    return ipk_path


if __name__ == "__main__":
    build_ipk()
