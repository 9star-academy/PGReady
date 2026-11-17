#!/usr/bin/env bash
set -euo pipefail

die() {
    printf '错误：%s\n' "$*" >&2
    exit 1
}

# Keep the standalone entry points aligned with internal/host/packages.go.
check_platform() {
    local kernel machine arch
    kernel=$(uname -s)
    [[ $kernel == Linux ]] || die "检测到 ${kernel}。PGReady 仅支持 Linux。"
    machine=$(uname -m)
    case "$machine" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) die "检测到架构 ${machine}。PGReady 仅支持 Linux amd64 和 arm64。" ;;
    esac

    local os_release=/etc/os-release
    local support='PGReady 仅支持 Ubuntu 22.04、24.04、26.04 LTS 和 Debian 12、13。'
    [[ -e $os_release ]] || os_release=/usr/lib/os-release
    [[ -r $os_release ]] || die "无法读取 Linux 发行版信息。$support"

    local key value id= version_id=
    while IFS='=' read -r key value || [[ -n $key ]]; do
        case "$value" in
            \"*\"|\'*\') value=${value:1:${#value}-2} ;;
        esac
        case "$key" in
            ID) id=$value ;;
            VERSION_ID) version_id=$value ;;
        esac
    done < "$os_release"
    case "$id:$version_id" in
        ubuntu:22.04|ubuntu:24.04|ubuntu:26.04|debian:12|debian:13) printf '%s\n' "$arch" ;;
        *) die "检测到 ${id:-未知发行版} ${version_id:-未知版本}。$support" ;;
    esac
}

readonly version=v0.1.3
readonly installer_sha256=18b6d53a498e082f183695be054671755bf886ad85899eb6ce68c543788e6ddd
readonly upstream="https://github.com/9star-academy/PGReady/releases/download/$version/install.sh"

check_platform >/dev/null

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
