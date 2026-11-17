package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/9star-academy/PGReady/internal/command"
)

type Package struct {
	Major     string
	Installed string
	Candidate string
}

var (
	versionedPackage = regexp.MustCompile("^postgresql-([1-9][0-9]*(?:\\.[0-9]+)?)$")
	debianVersion    = regexp.MustCompile("^[0-9][0-9A-Za-z.+:~-]*$")
)

const updateHint = "请先运行 sudo apt-get update，再检查已配置的 APT 软件源"

func (s Service) operatingSystem() (string, error) {
	files := s.filesystem()
	data, err := fs.ReadFile(files, "etc/os-release")
	if errors.Is(err, fs.ErrNotExist) {
		data, err = fs.ReadFile(files, "usr/lib/os-release")
	}
	if err != nil {
		return "", fmt.Errorf("读取系统版本: %w", err)
	}

	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else {
			value = strings.Trim(value, "'")
		}
		values[key] = value
	}
	id, version, description := values["ID"], values["VERSION_ID"], values["PRETTY_NAME"]
	if description == "" {
		description = strings.TrimSpace(id + " " + version)
	}
	if (id != "ubuntu" || version != "24.04") && (id != "debian" || version != "13") {
		return "", fmt.Errorf("APT 安装与版本检查仅支持 Ubuntu 24.04 LTS 或 Debian 13，检测到 %q", description)
	}
	return description, nil
}

func (s Service) Environment(ctx context.Context) (string, error) {
	description, err := s.operatingSystem()
	if err != nil {
		return "", err
	}
	out, err := command.Output(ctx, s.Runner, command.Call{
		Name: "/usr/bin/apt-cache", Args: []string{"policy"},
	})
	if err != nil {
		return "", fmt.Errorf("检查 APT 软件源 (%s): %w", updateHint, err)
	}

	origins := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if host, found := strings.CutPrefix(strings.TrimSpace(line), "origin "); found {
			origins[host] = true
		}
	}
	names := make([]string, 0, len(origins))
	for name := range origins {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Sprintf("系统 %s\nAPT 缓存未列出来源主机。%s", description, updateHint), nil
	}
	return fmt.Sprintf("系统 %s\nAPT 来源主机 %s", description, strings.Join(names, "、")), nil
}

func (s Service) Packages(ctx context.Context) ([]Package, error) {
	if _, err := s.operatingSystem(); err != nil {
		return nil, err
	}
	out, err := command.Output(ctx, s.Runner, command.Call{
		Name: "/usr/bin/apt-cache", Args: []string{"pkgnames", "postgresql-"},
	})
	if err != nil {
		return nil, fmt.Errorf("获取 PostgreSQL 软件包 (%s): %w", updateHint, err)
	}
	seen := make(map[string]bool)
	var names []string
	for _, name := range strings.Fields(out) {
		if versionedPackage.MatchString(name) && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := names[i], names[j]
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	if len(names) == 0 {
		return nil, fmt.Errorf("APT 缓存中没有 PostgreSQL 服务端软件包。%s", updateHint)
	}
	policy, err := s.packagePolicy(ctx, names)
	if err != nil {
		return nil, err
	}
	var candidates []Package
	for _, name := range names {
		if p := policy[strings.TrimPrefix(name, "postgresql-")]; p.Candidate != "" {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("APT 没有可安装的 PostgreSQL 服务端版本。%s", updateHint)
	}
	return candidates, nil
}

func (s Service) Package(ctx context.Context, major string) (Package, error) {
	if _, err := s.operatingSystem(); err != nil {
		return Package{}, err
	}
	name := "postgresql-" + major
	if !versionedPackage.MatchString(name) {
		return Package{}, fmt.Errorf("无效的 PostgreSQL 主版本 %q", major)
	}
	policy, err := s.packagePolicy(ctx, []string{name})
	if err != nil {
		return Package{}, err
	}
	p, ok := policy[major]
	if !ok {
		return Package{}, fmt.Errorf("APT 缓存中没有 %s。%s", name, updateHint)
	}
	return p, nil
}

func (s Service) packagePolicy(ctx context.Context, names []string) (map[string]Package, error) {
	out, err := command.Output(ctx, s.Runner, command.Call{
		Name: "/usr/bin/apt-cache", Args: append([]string{"policy"}, names...),
	})
	if err != nil {
		return nil, fmt.Errorf("读取 PostgreSQL 安装版本和候选版本 (%s): %w", updateHint, err)
	}
	return parsePolicy(out)
}

func parsePolicy(text string) (map[string]Package, error) {
	packages := make(map[string]Package)
	var current *Package
	for _, line := range strings.Split(text, "\n") {
		if name, isHeader := strings.CutSuffix(line, ":"); isHeader {
			// APT may attach a Debian architecture after the package name.
			name, _, _ = strings.Cut(name, ":")
			match := versionedPackage.FindStringSubmatch(name)
			if match != nil {
				if current != nil {
					packages[current.Major] = *current
				}
				current = &Package{Major: match[1]}
			}
			continue
		}
		key, version, found := strings.Cut(strings.TrimSpace(line), ": ")
		if current == nil || !found || (key != "Installed" && key != "Candidate") || version == "(none)" {
			continue
		}
		if !debianVersion.MatchString(version) {
			return nil, fmt.Errorf("APT 返回 %s 的无效软件包版本 %q", current.Major, version)
		}
		if key == "Installed" {
			current.Installed = version
		} else {
			current.Candidate = version
		}
	}
	if current != nil {
		packages[current.Major] = *current
	}
	return packages, nil
}

func (s Service) Install(ctx context.Context, major string, out, errOut io.Writer) error {
	p, err := s.Package(ctx, major)
	if err != nil {
		return err
	}
	if p.Installed != "" {
		return fmt.Errorf("已安装 postgresql-%s %s，首装功能仅适用于没有 PostgreSQL 的主机", p.Major, p.Installed)
	}
	clusters, err := s.Clusters(ctx)
	if err != nil {
		return err
	}
	if len(clusters) != 0 {
		return fmt.Errorf("本机已有 PostgreSQL 实例，首装功能仅适用于没有 PostgreSQL 的主机")
	}
	if p.Candidate == "" {
		return fmt.Errorf("postgresql-%s 没有可安装的候选版本。%s", major, updateHint)
	}
	if err := s.Runner.Run(ctx, command.Call{
		Name: "/usr/bin/apt-get", Args: []string{
			"--yes", "--no-remove", "--no-upgrade", "install", "postgresql-" + p.Major + "=" + p.Candidate,
		}, AsRoot: true, Stdout: out, Stderr: errOut,
	}); err != nil {
		return fmt.Errorf("安装 postgresql-%s: %w", major, err)
	}
	clusters, err = s.Clusters(ctx)
	if err != nil {
		return fmt.Errorf("APT 已完成安装命令，软件包的实例验证失败: %w", err)
	}
	for _, c := range clusters {
		if c.Version == major {
			return nil
		}
	}
	return fmt.Errorf("APT 已完成安装命令，软件包未生成可识别的 PostgreSQL %s 实例，请检查 postgresql-common 配置和安装输出", major)
}
