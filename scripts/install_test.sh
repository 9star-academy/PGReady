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

printf 'PGReady 安装平台检查通过。\n'
