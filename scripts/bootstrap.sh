#!/usr/bin/env bash
set -euo pipefail

die() {
    printf '错误：%s\n' "$*" >&2
    exit 1
}

readonly version=v0.1.1
readonly installer_sha256=2c5bf221d1c7858093ef18e24f17271f7f96dad935ec249cab2d8e44849f8489
readonly upstream="https://github.com/9star-academy/PGReady/releases/download/$version/install.sh"

mode=${PGREADY_DOWNLOAD_MODE:-auto}
mirror=${PGREADY_GITHUB_MIRROR:-https://gh-proxy.com}
mirror_pattern='^https://[a-zA-Z0-9.-]+(:[0-9]+)?(/[a-zA-Z0-9._~/-]*)?$'
[[ $mirror =~ $mirror_pattern ]] || die 'PGREADY_GITHUB_MIRROR 必须为不含凭据、查询参数的 HTTPS 镜像前缀。'
while [[ $mirror == */ ]]; do
    mirror=${mirror%/}
done

case "$mode" in
    auto) candidates=("$upstream" "$mirror/$upstream") ;;
    cn) candidates=("$mirror/$upstream" "$upstream") ;;
    direct) candidates=("$upstream") ;;
    *) die 'PGREADY_DOWNLOAD_MODE 必须为 auto、cn 或 direct。' ;;
esac
export PGREADY_DOWNLOAD_MODE="$mode" PGREADY_GITHUB_MIRROR="$mirror"

for tool in curl sha256sum mktemp; do
    command -v "$tool" >/dev/null 2>&1 || die "缺少命令 $tool。"
done

installer=$(mktemp)
trap 'rm -f -- "$installer"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for url in "${candidates[@]}"; do
    printf '[PGReady] 下载安装脚本：%s\n' "$url" >&2
    if ! curl --disable --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --connect-timeout 8 --max-time 30 --output "$installer" "$url"; then
        continue
    fi
    if [[ $(sha256sum < "$installer") != "$installer_sha256  -" ]]; then
        printf '[PGReady] 安装脚本 SHA-256 校验失败：%s\n' "$url" >&2
        continue
    fi
    bash "$installer" "$version"
    exit 0
done

die '所有线路均未取得通过校验的安装脚本。'
