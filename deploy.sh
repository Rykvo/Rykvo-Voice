#!/usr/bin/env bash
set -Eeuo pipefail
[[ $EUID == 0 ]] || { echo '请使用 sudo 或 root 运行'; exit 1; }
case "${1:-install}" in install|update|uninstall|status) ;; *) echo '操作无效'; exit 1;; esac
if [[ -x /usr/local/bin/rykvo && "${1:-install}" != install ]]; then
    exec /usr/local/bin/rykvo "$@"
fi
# shellcheck disable=SC1091
source /etc/os-release
case "$ID:$VERSION_ID" in ubuntu:24.04|ubuntu:26.04|debian:13) ;; *) echo '支持 Ubuntu 24.04/26.04、Debian 13'; exit 1;; esac
case "$(uname -m)" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; *) echo '支持 amd64/arm64'; exit 1;; esac
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y --no-install-recommends ca-certificates curl python3
temporary=$(mktemp -d /var/tmp/rykvo-bootstrap-XXXXXXXX)
chmod 700 "$temporary"
trap 'rm -rf --one-file-system -- "$temporary"' EXIT
curl --proto '=https' --tlsv1.2 -fsSL --retry 3 --max-time 120 \
    https://raw.githubusercontent.com/Rykvo/Rykvo-Voice/main/deploy/release.py -o "$temporary/release.py"
bundle=$(printf '\n' | python3 "$temporary/release.py" fetch "$temporary" "$arch")
bash "$bundle/install.sh" "${1:-install}"
