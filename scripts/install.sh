#!/usr/bin/env bash
set -euo pipefail

die() {
    printf '错误：%s\n' "$*" >&2
    exit 1
}

download() {
    curl --disable --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --connect-timeout 15 --max-time 120 --retry 2 \
        --output "$1" "$2"
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

    local asset="pgready-linux-$arch"
    local base="https://github.com/9star-academy/PGReady/releases/download/$version"
    download "$temporary_directory/SHA256SUMS" "$base/SHA256SUMS" || die '下载 SHA256SUMS 失败。'

    local record hash filename amd64_hash= arm64_hash=
    local record_pattern='^([0-9A-Fa-f]{64}) [ *](pgready-linux-amd64|pgready-linux-arm64)$'
    while IFS= read -r record || [[ -n $record ]]; do
        [[ $record =~ $record_pattern ]] || die 'SHA256SUMS 记录格式无效。'
        hash=${BASH_REMATCH[1]}
        filename=${BASH_REMATCH[2]}
        case "$filename" in
            pgready-linux-amd64)
                [[ -z $amd64_hash ]] || die 'SHA256SUMS 包含重复的 amd64 记录。'
                amd64_hash=$hash
                ;;
            pgready-linux-arm64)
                [[ -z $arm64_hash ]] || die 'SHA256SUMS 包含重复的 arm64 记录。'
                arm64_hash=$hash
                ;;
        esac
    done < "$temporary_directory/SHA256SUMS"
    [[ -n $amd64_hash && -n $arm64_hash ]] || die 'SHA256SUMS 缺少架构记录。'

    local checksum=$amd64_hash
    if [[ $arch == arm64 ]]; then
        checksum=$arm64_hash
    fi
    download "$temporary_directory/$asset" "$base/$asset" || die "下载 $asset 失败。"
    (
        cd -- "$temporary_directory"
        printf '%s  %s\n' "$checksum" "$asset" | sha256sum --check --strict --status -
    ) || die 'SHA-256 校验失败。'

    local binary="$temporary_directory/$asset"
    chmod -- 0755 "$binary"
    local installed_version
    installed_version=$("$binary" --version) || die '无法执行版本检查。'
    [[ $installed_version == "pgready $version" ]] || die "程序版本与发布标签 $version 不一致。"

    local destination=/usr/local/bin/pgready
    install_binary "$binary" "$checksum" "$destination" || die "安装到 $destination 失败。"
    printf '已安装 pgready %s 到 %s\n' "$version" "$destination"
}

main "$@"
