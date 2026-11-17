#!/usr/bin/env bash
set -euo pipefail

die() {
    printf '错误：%s\n' "$*" >&2
    exit 1
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
directory=${destination%/*}
staged=
trap 'if [[ -n $staged ]]; then rm -f -- "$staged"; fi' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ ! -d $directory ]]; then
    install -d -- "$directory"
fi
if [[ -L $destination || ( -e $destination && ! -f $destination ) ]]; then
    printf '错误：安装目标必须是普通文件或尚未创建。\n' >&2
    exit 1
fi
staged=$(mktemp "$directory/.pgready-install.XXXXXX")
install -m 0755 -- "$source_file" "$staged"
(
    cd -- "$directory"
    printf '%s  %s\n' "$checksum" "${staged##*/}" | sha256sum --check --strict --status -
)
mv -fT -- "$staged" "$destination"
staged=
PGREADY_REPLACE
}

main() {
    [[ $# -eq 1 ]] || die '用法：install.sh vX.Y.Z'
    local version=$1
    [[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die '版本号格式必须为 vX.Y.Z。'
    local mode=${PGREADY_DOWNLOAD_MODE:-auto}
    local mirror=${PGREADY_GITHUB_MIRROR:-https://gh-proxy.com}
    local mirror_pattern='^https://[a-zA-Z0-9.-]+(:[0-9]+)?(/[a-zA-Z0-9._~/-]*)?$'
    [[ $mirror =~ $mirror_pattern ]] || die 'PGREADY_GITHUB_MIRROR 必须为不含凭据、查询参数的 HTTPS 镜像前缀。'
    while [[ $mirror == */ ]]; do
        mirror=${mirror%/}
    done
    local -a download_bases
    local base="https://github.com/9star-academy/PGReady/releases/download/$version"
    case "$mode" in
        auto) download_bases=("$base" "$mirror/$base") ;;
        cn) download_bases=("$mirror/$base" "$base") ;;
        direct) download_bases=("$base") ;;
        *) die 'PGREADY_DOWNLOAD_MODE 必须为 auto、cn 或 direct。' ;;
    esac

    [[ $(uname -s) == Linux ]] || die '仅支持 Linux。'

    local arch
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) die '仅支持 Linux amd64 和 arm64。' ;;
    esac
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
    install_binary "$binary" "$checksum" "$destination" || die "安装到 $destination 失败。"
    printf '已安装 pgready %s 到 %s\n' "$version" "$destination"
}

main "$@"
