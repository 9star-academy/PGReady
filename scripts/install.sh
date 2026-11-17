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

set_download_bases() {
    local upstream=$1 mode=${PGREADY_DOWNLOAD_MODE:-auto}
    local mirror=${PGREADY_GITHUB_MIRROR:-https://gh-proxy.com}
    local mirror_pattern='^https://[a-zA-Z0-9.-]+(:[0-9]+)?(/[a-zA-Z0-9._~/-]*)?$'
    [[ $mirror =~ $mirror_pattern ]] || die 'PGREADY_GITHUB_MIRROR 必须为不含凭据、查询参数的 HTTPS 镜像前缀。'
    while [[ $mirror == */ ]]; do
        mirror=${mirror%/}
    done
    case "$mode" in
        auto) download_bases=("$upstream" "$mirror/$upstream") ;;
        cn) download_bases=("$mirror/$upstream" "$upstream") ;;
        direct) download_bases=("$upstream") ;;
        *) die 'PGREADY_DOWNLOAD_MODE 必须为 auto、cn 或 direct。' ;;
    esac
}

download() {
    curl --disable --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --connect-timeout 8 --max-time 120 --retry 1 --retry-delay 1 \
        --speed-time 20 --speed-limit 1024 \
        --output "$1" "$2"
}

download_verified() {
    local file=$1 verify=$2
    shift 2
    local base url
    for base in "${download_bases[@]}"; do
        url="$base/${file##*/}"
        rm -f -- "$file" || return 1
        printf '[PGReady] 下载：%s\n' "$url" >&2
        if ! download "$file" "$url"; then
            printf '[PGReady] 下载失败：%s\n' "$url" >&2
        elif "$verify" "$file" "$@"; then
            return 0
        else
            printf '[PGReady] 文件未通过校验：%s\n' "$url" >&2
        fi
    done
    return 1
}

manifest_checksum() {
    local file=$1 asset=$2
    local record hash filename amd64_hash= arm64_hash=
    local record_pattern='^([0-9A-Fa-f]{64}) [ *](pgready-linux-amd64|pgready-linux-arm64)$'
    while IFS= read -r record || [[ -n $record ]]; do
        [[ $record =~ $record_pattern ]] || return 1
        hash=${BASH_REMATCH[1]}
        filename=${BASH_REMATCH[2]}
        case "$filename" in
            pgready-linux-amd64)
                [[ -z $amd64_hash ]] || return 1
                amd64_hash=$hash
                ;;
            pgready-linux-arm64)
                [[ -z $arm64_hash ]] || return 1
                arm64_hash=$hash
                ;;
        esac
    done < "$file" || return 1
    [[ -n $amd64_hash && -n $arm64_hash ]] || return 1
    if [[ $asset == pgready-linux-arm64 ]]; then
        printf '%s\n' "$arm64_hash"
    else
        printf '%s\n' "$amd64_hash"
    fi
}

verify_binary() {
    local binary=$1 checksum=$2 version=$3 installed_version
    if ! (
        cd -- "${binary%/*}" || exit 1
        printf '%s  %s\n' "$checksum" "${binary##*/}" | sha256sum --check --strict --status -
    ); then
        printf '[PGReady] SHA-256 校验失败。\n' >&2
        return 1
    fi
    chmod -- 0755 "$binary" || return 1
    if ! installed_version=$("$binary" --version) || [[ $installed_version != "pgready $version" ]]; then
        printf '[PGReady] 程序版本与发布标签 %s 不一致。\n' "$version" >&2
        return 1
    fi
}

install_binary() {
    local -a bash_command=(/bin/bash)
    if (( EUID != 0 )); then
        command -v sudo >/dev/null 2>&1 || die '安装到 /usr/local/bin 需要 sudo。'
        bash_command=(sudo -- /bin/bash)
    fi

    "${bash_command[@]}" -s -- "$@" <<'PGREADY_REPLACE'
set -euo pipefail
source_file=$1
checksum=$2
destination=$3
running=$4
directory=${destination%/*}
staged=
trap 'if [[ -n $staged ]]; then rm -f -- "$staged"; fi' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ ! -d $directory ]]; then
    install -d -- "$directory"
fi
check_destination() {
    if [[ -L $destination || ( -e $destination && ! -f $destination ) ]]; then
        printf '错误：安装目标必须是普通文件或尚未创建。\n' >&2
        exit 1
    fi
    if [[ -n $running && ! $destination -ef $running ]]; then
        printf '错误：PGReady 安装目标已变更，请重新运行 pgready update。\n' >&2
        exit 1
    fi
}
check_destination
staged=$(mktemp "$directory/.pgready-install.XXXXXX")
install -m 0755 -- "$source_file" "$staged"
(
    cd -- "$directory"
    printf '%s  %s\n' "$checksum" "${staged##*/}" | sha256sum --check --strict --status -
)
check_destination
mv -fT -- "$staged" "$destination"
staged=
PGREADY_REPLACE
}

main() {
    local -a download_bases
    if [[ $# -eq 1 && $1 == --release-sources ]]; then
        check_platform >/dev/null
        set_download_bases 'https://api.github.com/repos/9star-academy/PGReady/releases/latest'
        printf '%s\n' "${download_bases[@]}"
        return
    fi
    local running=
    if [[ $# -eq 3 && $1 == --update && $3 =~ ^/proc/[1-9][0-9]*/exe$ ]]; then
        running=$3
        set -- "$2"
    fi
    [[ $# -eq 1 ]] || die '用法：install.sh vX.Y.Z'
    local version=$1
    [[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die '版本号格式必须为 vX.Y.Z。'
    set_download_bases "https://github.com/9star-academy/PGReady/releases/download/$version"

    local arch
    arch=$(check_platform)
    local tool
    for tool in curl sha256sum mktemp chmod install mv rm; do
        command -v "$tool" >/dev/null 2>&1 || die "缺少命令 $tool。"
    done

    temporary_directory=$(mktemp -d)
    trap 'rm -rf -- "$temporary_directory"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM

    local asset="pgready-linux-$arch" checksum
    checksum=$(download_verified "$temporary_directory/SHA256SUMS" manifest_checksum "$asset") \
        || die '所有线路均未取得有效的 SHA256SUMS。'
    local binary="$temporary_directory/$asset"
    download_verified "$binary" verify_binary "$checksum" "$version" \
        || die "所有线路均未取得通过校验的 ${asset}。"

    local destination=/usr/local/bin/pgready
    install_binary "$binary" "$checksum" "$destination" "$running" || die "安装到 $destination 失败。"
    if [[ -n $running ]]; then
        printf '已更新 PGReady 到 %s。\n' "$version"
    else
        printf '已安装 pgready %s 到 %s\n' "$version" "$destination"
    fi
}

main "$@"
