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
    local timeout_seconds=120 retries=1
    if [[ ${1##*/} == SHA256SUMS ]] && (( ${#download_bases[@]} > 1 )); then
        # Give the preferred source a short chance to supply the checksum first.
        timeout_seconds=8
        retries=0
    fi
    # Replace the background shell so stopping the job also stops curl.
    exec curl --disable --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --connect-timeout 8 --max-time "$timeout_seconds" --retry "$retries" --retry-delay 1 \
        --speed-time 20 --speed-limit 1024 \
        --output "$1" "$2"
}

start_download() {
    local index=$1 file=$2 url=$3
    printf '[PGReady] 下载：%s\n' "$url" >&2
    download "$file" "$url" 2>"$file.error" &
    download_pids[index]=$!
}

stop_downloads() {
    local pid active_jobs
    active_jobs=$'\n'$(jobs -pr)$'\n'
    for pid in "${download_pids[@]}"; do
        [[ -n $pid ]] || continue
        if [[ $active_jobs == *$'\n'"$pid"$'\n'* ]]; then
            kill "$pid" 2>/dev/null || true
        fi
        wait "$pid" 2>/dev/null || true
    done
    download_pids=("" "")
}

download_verified() {
    local file=$1 verify=$2
    shift 2
    local route_count=${#download_bases[@]} i dir
    local -a paths urls
    for ((i = 0; i < route_count; i++)); do
        dir="${file%/*}/route-$i"
        mkdir -p -- "$dir" || return 1
        paths[i]="$dir/${file##*/}"
        urls[i]="${download_bases[i]}/${file##*/}"
    done

    local started=1 active pid active_jobs elapsed enable_backup
    local progress_interval=4 minimum_bytes_per_second=$((64 * 1024))
    local last_sample=$SECONDS previous_bytes=0 bytes host
    start_download 0 "${paths[0]}" "${urls[0]}"
    while :; do
        active_jobs=$'\n'$(jobs -pr)$'\n'
        active=0
        for ((i = 0; i < started; i++)); do
            pid=${download_pids[i]}
            [[ -n $pid ]] || continue
            if [[ $active_jobs == *$'\n'"$pid"$'\n'* ]]; then
                active=$((active + 1))
                continue
            fi

            download_pids[i]=
            if ! wait "$pid" 2>/dev/null; then
                cat -- "${paths[i]}.error" >&2
                printf '[PGReady] 下载失败：%s\n' "${urls[i]}" >&2
            elif "$verify" "${paths[i]}" "$@"; then
                stop_downloads
                mv -f -- "${paths[i]}" "$file"
                return $?
            else
                printf '[PGReady] 文件未通过校验：%s\n' "${urls[i]}" >&2
            fi
        done

        if (( active == 0 && started == route_count )); then
            return 1
        fi
        enable_backup=0
        if (( active == 0 )); then
            enable_backup=1
        fi
        elapsed=$((SECONDS - last_sample))
        if (( elapsed >= progress_interval )); then
            for ((i = 0; i < started; i++)); do
                [[ -n ${download_pids[i]} ]] || continue
                bytes=$(wc -c 2>/dev/null < "${paths[i]}") || bytes=0
                host=${urls[i]#https://}
                printf '[PGReady] %s 已下载 %d KiB\n' "${host%%/*}" "$((bytes / 1024))" >&2
                if (( i == 0 && bytes - previous_bytes < elapsed * minimum_bytes_per_second )) &&
                    [[ ${file##*/} != SHA256SUMS ]]; then
                    enable_backup=1
                fi
                if (( i == 0 )); then
                    previous_bytes=$bytes
                fi
            done
            last_sample=$SECONDS
        fi
        if (( enable_backup && started < route_count )); then
            printf '[PGReady] 启用备用下载线路。\n' >&2
            start_download "$started" "${paths[started]}" "${urls[started]}"
            started=$((started + 1))
        fi
        sleep 0.25
    done
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
    for tool in curl sha256sum mktemp chmod install mkdir mv rm cat wc sleep; do
        command -v "$tool" >/dev/null 2>&1 || die "缺少命令 $tool。"
    done

    temporary_directory=$(mktemp -d)
    download_pids=("" "")
    trap 'stop_downloads; rm -rf -- "$temporary_directory"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM

    local asset="pgready-linux-$arch" manifest="$temporary_directory/SHA256SUMS" checksum
    download_verified "$manifest" manifest_checksum "$asset" > /dev/null \
        || die '所有线路均未取得有效的 SHA256SUMS。'
    checksum=$(manifest_checksum "$manifest" "$asset")
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
