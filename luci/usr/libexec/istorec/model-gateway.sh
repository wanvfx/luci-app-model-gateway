#!/bin/sh
# Model Gateway iStoreOS lifecycle script
# Native binary (no Docker)

ACTION=${1}
shift 1

# Detect correct binary name by architecture
detect_binary() {
    local arch=$(uname -m)
    case "$arch" in
        aarch64|arm64)
            echo "/usr/bin/model-gatewayd-arm64"
            ;;
        x86_64|amd64)
            echo "/usr/bin/model-gatewayd"
            ;;
        *)
            echo "/usr/bin/model-gatewayd"
            ;;
    esac
}

do_install() {
  local bin=$(detect_binary)
  chmod +x "$bin"

  echo "Install OK"
}

do_upgrade() {
  do_install
}

usage() {
  echo "usage: $0 sub-command"
  echo "where sub-command is one of:"
  echo "      install                 Install the model-gateway"
  echo "      upgrade                 Upgrade the model-gateway"
  echo "      rm/start/stop/restart   Remove/Start/Stop/Restart the model-gateway"
  echo "      status                  Model Gateway status"
  echo "      port                    Model Gateway port"
}

case ${ACTION} in
  "install")
    do_install
  ;;
  "autoconf")
    # iStoreOS 安装时若指定了外部存储盘，is-opkg 会把真实挂载点（如 /mnt/sda1，具体盘名随设备而定）
    # 作为 base path 传入 $1，这里把它落地为 config_path，使应用真正跑在外部盘而非内置闪存。
    # 注意：此处使用的是 is-opkg 传入的真实挂载点，并非写死盘名；页面内修改路径则由 CBI 动态枚举挂载点。
    local base="$1"
    if [ -n "$base" ]; then
      local cpdir="$base/Configs/model-gateway"
      mkdir -p "$cpdir"
      cp /etc/config/model-gateway "$cpdir/model-gateway" 2>/dev/null
      uci -q set model-gateway.settings.config_path="$cpdir"
      uci commit model-gateway
      echo "AUTOCONF OK -> $cpdir"
    else
      echo "AUTOCONF skipped: no base path"
    fi
  ;;
  "upgrade")
    do_upgrade
  ;;
  "rm")
    /etc/init.d/model-gateway stop >/dev/null 2>&1 || true
    /etc/init.d/model-gateway disable >/dev/null 2>&1 || true
    rm -f /usr/bin/model-gatewayd /usr/bin/model-gatewayd-arm64
    echo "Removed"
  ;;
  "start" | "stop" | "restart")
    /etc/init.d/model-gateway ${ACTION}
  ;;
  "status")
    /etc/init.d/model-gateway status
  ;;
  "port")
    # P3-1: 用具名段（config model-gateway 'settings'），与 CBI/init.d 引用一致，避免 @settings[0] 位置匹配脆弱
    uci -q get model-gateway.settings.port 2>/dev/null || echo "8080"
  ;;
  *)
    usage
    exit 1
  ;;
esac
