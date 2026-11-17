#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "$0")"
test_dir=$(mktemp -d)
trap 'rm -rf -- "$test_dir"' EXIT
mkdir "$test_dir/bin"

cat > "$test_dir/bin/uname" <<'EOF'
#!/usr/bin/env bash
case "$1" in
    -s) printf '%s\n' "$PGREADY_TEST_KERNEL" ;;
    -m) printf '%s\n' "$PGREADY_TEST_MACHINE" ;;
esac
EOF

cat > "$test_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' '下载已调用' >&2
exit 1
EOF
chmod +x "$test_dir/bin/uname" "$test_dir/bin/curl"

fail() {
    printf '%s: %s\n%s\n' "$script" "$1" "$output" >&2
    exit 1
}

assert_platform() {
    local kernel=$1 machine=$2 expected=$3
    if output=$(PATH="$test_dir/bin:$PATH" PGREADY_DOWNLOAD_MODE=direct \
        PGREADY_TEST_KERNEL="$kernel" PGREADY_TEST_MACHINE="$machine" \
        bash "$test_dir/$script" v0.0.0 2>&1); then
        fail '预期安装返回非零状态（测试下载始终失败）'
    fi
    [[ $output == *"$expected"* ]] || fail "缺少预期提示：$expected"
    if [[ $expected != '下载已调用' && $output == *'下载已调用'* ]]; then
        fail '平台检查失败后仍调用了下载'
    fi
}

for script in bootstrap.sh install.sh; do
    bash -n "$script"
    sed -e "s|/etc/os-release|$test_dir/os-release|g" \
        -e "s|/usr/lib/os-release|$test_dir/os-release-fallback|g" \
        "$script" > "$test_dir/$script"
    rm -f -- "$test_dir/os-release" "$test_dir/os-release-fallback"

    assert_platform Linux x86_64 '无法读取 Linux 发行版信息'
    printf 'ID=debian\nVERSION_ID=13\n' > "$test_dir/os-release-fallback"
    assert_platform Linux aarch64 '下载已调用'
    printf 'ID=ubuntu\nVERSION_ID="24.04"' > "$test_dir/os-release"
    assert_platform Linux x86_64 '下载已调用'
    assert_platform Linux riscv64 'riscv64'
    assert_platform Darwin arm64 'Darwin'
    printf 'ID=ubuntu\nVERSION_ID="22.04"\n' > "$test_dir/os-release"
    assert_platform Linux x86_64 '下载已调用'
    printf 'ID=ubuntu\nVERSION_ID="26.04"\n' > "$test_dir/os-release"
    assert_platform Linux aarch64 '下载已调用'
    printf 'ID=debian\nVERSION_ID=12\n' > "$test_dir/os-release"
    assert_platform Linux aarch64 '下载已调用'
    support='PGReady 仅支持 Ubuntu 22.04、24.04、26.04 LTS 和 Debian 12、13。'
    printf 'ID=debian\nVERSION_ID=11\n' > "$test_dir/os-release"
    assert_platform Linux aarch64 "检测到 debian 11。$support"
    printf 'ID=rocky\nVERSION_ID="9.6"\n' > "$test_dir/os-release"
    assert_platform Linux aarch64 "检测到 rocky 9.6。$support"
    printf 'ID=debian\n' > "$test_dir/os-release"
    assert_platform Linux aarch64 "检测到 debian 未知版本。$support"
done

script=install.sh
printf 'ID=debian\nVERSION_ID=12\n' > "$test_dir/os-release"
latest='https://api.github.com/repos/9star-academy/PGReady/releases/latest'
mirror='https://mirror.example:8443/github'
for mode in auto cn direct; do
    case "$mode" in
        auto) expected=$(printf '%s\n' "$latest" "$mirror/$latest") ;;
        cn) expected=$(printf '%s\n' "$mirror/$latest" "$latest") ;;
        direct) expected=$latest ;;
    esac
    output=$(PATH="$test_dir/bin:$PATH" PGREADY_TEST_KERNEL=Linux PGREADY_TEST_MACHINE=aarch64 \
        PGREADY_DOWNLOAD_MODE="$mode" PGREADY_GITHUB_MIRROR="$mirror/" \
        bash "$test_dir/install.sh" --release-sources) || fail "无法读取 $mode 模式的查询地址"
    [[ $output == "$expected" ]] || fail "$mode 模式的查询地址顺序错误"
done

sed '/^main "\$@"$/d' install.sh > "$test_dir/downloads.sh"
cat > "$test_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -eu
while [[ $# -gt 1 ]]; do
    if [[ $1 == --output ]]; then
        file=$2
        shift 2
    else
        shift
    fi
done
printf '%s\n' "$1" >> "$PGREADY_TEST_REQUESTS"
case "$PGREADY_TEST_TRANSFER:$1" in
    slow:https://direct.example/*)
        printf '%s\n' "$$" > "$PGREADY_TEST_SLOW_PID"
        printf 'partial\n' > "$file"
        exec sleep 15
        ;;
    invalid:https://direct.example/*)
        printf 'invalid\n' > "$file"
        ;;
    slow_manifest:https://direct.example/SHA256SUMS)
        sleep 5
        printf 'verified\n' > "$file"
        ;;
    mirror_offline:https://direct.example/*)
        printf 'partial\n' > "$file"
        sleep 5
        printf 'verified\n' > "$file"
        ;;
    mirror_offline:https://mirror.example/*)
        exit 1
        ;;
    *) printf 'verified\n' > "$file" ;;
esac
EOF

assert_download() {
    local transfer=$1 wanted_requests=$2 filename=${3:-asset} start=$SECONDS
    local work="$test_dir/$transfer"
    mkdir "$work"
    output=$(PATH="$test_dir/bin:$PATH" PGREADY_TEST_TRANSFER="$transfer" \
        PGREADY_TEST_REQUESTS="$work/requests" PGREADY_TEST_SLOW_PID="$work/pid" \
        bash -eu -c '
            source "$1"
            download_bases=(https://direct.example https://mirror.example)
            download_pids=("" "")
            verify() { grep -Fxq verified "$1"; }
            download_verified "$2/$3" verify
            verify "$2/$3"
        ' bash "$test_dir/downloads.sh" "$work" "$filename" 2>&1) || fail "下载场景失败：$transfer"
    [[ $(wc -l < "$work/requests") -eq $wanted_requests ]] || fail "下载线路数错误：$transfer"
    if [[ $transfer == slow ]]; then
        [[ $output == *'已下载'* && $output == *'启用备用下载线路'* ]] || fail '缺少进度或切换提示'
        (( SECONDS - start < 12 )) || fail '慢线路未及时启用备用线路'
        if kill -0 "$(cat "$work/pid")" 2>/dev/null; then
            fail '胜出后慢线路仍在运行'
        fi
    fi
}

assert_download fast 1
assert_download slow 2
assert_download invalid 2
assert_download mirror_offline 2
assert_download slow_manifest 1 SHA256SUMS

printf 'PGReady 安装平台、更新线路和备用下载检查通过。\n'
