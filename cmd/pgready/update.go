package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/9star-academy/PGReady/scripts"
)

const installPath = "/usr/local/bin/pgready"

func update(ctx context.Context, currentTag string, out, errOut io.Writer) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("pgready update 仅支持 Linux")
	}
	current, err := parseReleaseVersion(currentTag)
	if err != nil {
		return fmt.Errorf("当前版本 %q 无法自动更新，请先运行正式版安装命令", currentTag)
	}

	// Read the running inode even if another process replaces the installed path.
	running, err := os.Open("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("读取正在运行的 PGReady 程序: %w", err)
	}
	defer running.Close()
	original, err := running.Stat()
	if err != nil {
		return fmt.Errorf("读取正在运行的 PGReady 程序信息: %w", err)
	}
	installed, err := os.Lstat(installPath)
	if err != nil || !installed.Mode().IsRegular() || !os.SameFile(installed, original) {
		return fmt.Errorf("请通过 %s 运行已安装的 PGReady 来更新", installPath)
	}

	sources := installerCommand(ctx, "--release-sources")
	sources.Stderr = errOut
	sourceURLs, err := sources.Output()
	if err != nil {
		return fmt.Errorf("读取更新下载设置: %w", err)
	}
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || req.URL.User != nil || len(via) >= 10 {
				return fmt.Errorf("无效的发布查询跳转")
			}
			return nil
		},
	}
	release, err := latestRelease(ctx, client, strings.Fields(string(sourceURLs)))
	if err != nil {
		return err
	}
	switch slices.Compare(current[:], release.version[:]) {
	case 0:
		fmt.Fprintf(out, "PGReady 已是最新正式版 %s。\n", currentTag)
		return nil
	case 1:
		fmt.Fprintf(out, "当前 PGReady %s 高于最新正式版 %s。\n", currentTag, release.Tag)
		return nil
	}

	// The installer rechecks this inode at the replacement boundary under sudo.
	if err := syscall.Flock(int(running.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("已有 PGReady 更新正在运行")
		}
		return fmt.Errorf("锁定当前 PGReady 程序: %w", err)
	}
	fmt.Fprintf(out, "发现 PGReady %s（当前 %s），开始更新。\n", release.Tag, currentTag)
	cmd := installerCommand(ctx, "--update", release.Tag, fmt.Sprintf("/proc/%d/exe", os.Getpid()))
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("更新 PGReady: %w", err)
	}
	return nil
}

func installerCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/bash", append([]string{"-p", "-s", "--"}, args...)...)
	cmd.Stdin = strings.NewReader(scripts.Installer)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

var releaseTagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func parseReleaseVersion(tag string) ([3]uint64, error) {
	var version [3]uint64
	parts := releaseTagPattern.FindStringSubmatch(tag)
	if parts == nil {
		return version, fmt.Errorf("无效的正式版版本号 %q", tag)
	}
	for i, part := range parts[1:] {
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return version, fmt.Errorf("无效的正式版版本号 %q", tag)
		}
		version[i] = number
	}
	return version, nil
}

type stableRelease struct {
	Tag        string `json:"tag_name"`
	Draft      *bool  `json:"draft"`
	Prerelease *bool  `json:"prerelease"`
	version    [3]uint64
}

func latestRelease(ctx context.Context, client *http.Client, sources []string) (stableRelease, error) {
	var failures []error
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return stableRelease{}, err
		}
		release, err := fetchRelease(ctx, client, source)
		if err == nil {
			return release, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", source, err))
	}
	if len(failures) == 0 {
		return stableRelease{}, fmt.Errorf("未配置正式版查询地址")
	}
	return stableRelease{}, fmt.Errorf("查询 PGReady 最新正式版失败: %w", errors.Join(failures...))
}

func fetchRelease(ctx context.Context, client *http.Client, source string) (stableRelease, error) {
	const maxMetadataSize = 1 << 20
	var release stableRelease
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return release, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return release, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return release, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataSize+1))
	if err != nil {
		return release, err
	}
	if len(data) > maxMetadataSize {
		return release, fmt.Errorf("发布信息超过大小限制")
	}
	if err := json.Unmarshal(data, &release); err != nil {
		return release, fmt.Errorf("无法解析发布信息: %w", err)
	}
	if release.Draft == nil || release.Prerelease == nil || *release.Draft || *release.Prerelease {
		return release, fmt.Errorf("发布信息不是正式版")
	}
	release.version, err = parseReleaseVersion(release.Tag)
	return release, err
}
