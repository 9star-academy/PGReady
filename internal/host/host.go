package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
)

type Service struct {
	Runner command.Runner
	files  fs.FS
}

const panelInstallDir = "www/server/pgsql"

func (s Service) filesystem() fs.FS {
	if s.files != nil {
		return s.files
	}
	return os.DirFS("/")
}

func (s Service) Clusters(ctx context.Context) ([]cluster.Cluster, error) {
	out, err := command.Output(ctx, s.Runner, command.Call{Name: "/usr/bin/pg_lsclusters"})
	if err == nil {
		clusters, parseErr := parseInventory(out)
		if parseErr != nil || len(clusters) > 0 {
			return clusters, parseErr
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("读取 PostgreSQL 实例列表 (pg_lsclusters): %w", err)
	}

	if err := s.checkNoExistingServer(ctx); err != nil {
		return nil, err
	}
	return nil, nil
}

func parseInventory(inventory string) ([]cluster.Cluster, error) {
	input := bufio.NewScanner(strings.NewReader(inventory))
	if !input.Scan() {
		return nil, fmt.Errorf("pg_lsclusters 未返回实例列表表头")
	}
	header := input.Text()
	if strings.Join(strings.Fields(header), " ") != "Ver Cluster Port Status Owner Data directory Log file" {
		return nil, fmt.Errorf("无法识别 pg_lsclusters 的实例列表表头")
	}

	// pg_lsclusters pads each column to the same width as the header. In
	// particular, splitting each row on whitespace would change paths.
	labels := []string{"Ver", "Cluster", "Port", "Status", "Owner", "Data directory", "Log file"}
	start := make([]int, len(labels))
	for i, label := range labels {
		start[i] = strings.Index(header, label)
	}
	var clusters []cluster.Cluster
	seen := make(map[string]bool)
	for line := 2; input.Scan(); line++ {
		row := input.Text()
		if len(row) <= start[6] {
			return nil, fmt.Errorf("pg_lsclusters 第 %d 行列数不完整", line)
		}
		parts := make([]string, len(labels))
		for i := 0; i < len(start)-1; i++ {
			next := start[i+1]
			if row[next-1] != ' ' {
				return nil, fmt.Errorf("pg_lsclusters 第 %d 行列边界不明确", line)
			}
			parts[i] = strings.TrimRight(row[start[i]:next-1], " ")
		}
		parts[6] = row[start[6]:]
		port, err := strconv.Atoi(parts[2])
		if err != nil || !filepath.IsAbs(parts[5]) {
			return nil, fmt.Errorf("pg_lsclusters 第 %d 行缺少有效端口或数据目录", line)
		}
		for _, value := range parts {
			if strings.ContainsAny(value, "\x00\x1b\r\t") {
				return nil, fmt.Errorf("pg_lsclusters 第 %d 行包含不明确的控制字符", line)
			}
		}
		c := cluster.Cluster{
			Version: parts[0], Name: parts[1], Port: port, Status: parts[3],
			Owner: parts[4], DataDir: parts[5], LogFile: parts[6],
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if seen[c.ID()] {
			return nil, fmt.Errorf("pg_lsclusters 重复报告集群 %s", c.ID())
		}
		seen[c.ID()] = true
		clusters = append(clusters, c)
	}
	if err := input.Err(); err != nil {
		return nil, fmt.Errorf("解析 pg_lsclusters 实例列表: %w", err)
	}
	return clusters, nil
}

func (s Service) checkNoExistingServer(ctx context.Context) error {
	files := s.filesystem()
	// Probe conventionally installed servers and residual data outside of the
	// postgresql-common registry.
	for _, location := range []string{
		"usr/bin/postgres", "usr/local/bin/postgres", "usr/local/pgsql",
		"opt/pgsql", "var/lib/pgsql",
	} {
		_, err := fs.Stat(files, location)
		if err == nil {
			return unregistered("/" + location)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("检查已有 PostgreSQL 文件: %w", err)
		}
	}

	if err := checkPanelInstallation(files); err != nil {
		return err
	}

	for _, root := range []string{"etc/postgresql", "var/lib/postgresql"} {
		entries, err := fs.ReadDir(files, root)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("检查已有 PostgreSQL 数据或配置: %w", err)
		}
		for _, entry := range entries {
			if versionedPackage.MatchString("postgresql-" + entry.Name()) {
				return unregistered("/" + path.Join(root, entry.Name()))
			}
		}
	}

	out, err := command.Output(ctx, s.Runner, command.Call{
		Name: "/usr/bin/dpkg-query",
		Args: []string{"--show", "--showformat=${Package}\t${db:Status-Abbrev}\n"},
	})
	if err != nil {
		return fmt.Errorf("检查已有 PostgreSQL 软件包 (dpkg-query): %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		name, status, found := strings.Cut(line, "\t")
		if found && versionedPackage.MatchString(name) && len(status) >= 2 && status[1] != 'n' {
			return unregistered(name)
		}
	}

	processes, err := fs.ReadDir(files, "proc")
	if err != nil {
		return fmt.Errorf("检查本机 PostgreSQL 进程 (/proc): %w", err)
	}
	for _, process := range processes {
		if _, err := strconv.ParseUint(process.Name(), 10, 64); err != nil {
			continue
		}
		comm, err := fs.ReadFile(files, path.Join("proc", process.Name(), "comm"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // The process can exit during discovery.
		}
		if err != nil {
			return fmt.Errorf("检查本机 PostgreSQL 进程: %w", err)
		}
		name := strings.TrimSpace(string(comm))
		if name == "postgres" || name == "postmaster" {
			return unregistered("PostgreSQL 进程 PID " + process.Name())
		}
	}
	return nil
}

func unregistered(evidence string) error {
	return fmt.Errorf("未找到已注册实例，但发现已有 PostgreSQL 痕迹 %q。请先确认其安装方式、数据目录和服务状态", evidence)
}

func checkPanelInstallation(files fs.FS) error {
	dataDirs, err := panelDataDirectories(files)
	if err != nil {
		return err
	}
	markers := []string{panelInstallDir + "/bin/postgres", panelInstallDir + "/bin/postmaster"}
	for _, dir := range dataDirs {
		markers = append(markers, path.Join(dir, "PG_VERSION"), path.Join(dir, "global/pg_control"))
	}

	var evidence []string
	for _, marker := range markers {
		info, err := fs.Stat(files, marker)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("检查宝塔 PostgreSQL 路径 %q: %w", "/"+marker, err)
		}
		detail := fmt.Sprintf("发现 %q", "/"+marker)
		if path.Base(marker) == "PG_VERSION" && info.Mode().IsRegular() && info.Size() <= 32 {
			data, err := fs.ReadFile(files, marker)
			version := strings.TrimSpace(string(data))
			if err == nil && versionedPackage.MatchString("postgresql-"+version) {
				detail = fmt.Sprintf("数据目录 %q 的 PostgreSQL 主版本为 %s", "/"+path.Dir(marker), version)
			}
		}
		evidence = append(evidence, detail)
	}
	if len(evidence) == 0 {
		return nil
	}
	return fmt.Errorf("未找到 APT 注册实例，但检测到宝塔 PostgreSQL 痕迹。%s。APT 首装已暂停。请核对数据目录和服务状态", strings.Join(evidence, "。"))
}

func panelDataDirectories(files fs.FS) ([]string, error) {
	dirs := []string{panelInstallDir + "/data", "www/server/data/pgsql"}
	const config = panelInstallDir + "/data_directory"
	info, err := fs.Stat(files, config)
	if errors.Is(err, fs.ErrNotExist) {
		return dirs, nil
	}
	if err != nil {
		return nil, fmt.Errorf("检查宝塔 PostgreSQL 数据目录配置: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return dirs, nil
	}
	data, err := fs.ReadFile(files, config)
	if err != nil {
		return nil, fmt.Errorf("读取宝塔 PostgreSQL 数据目录配置: %w", err)
	}
	configured := strings.TrimSpace(string(data))
	if path.IsAbs(configured) {
		dir := strings.TrimPrefix(path.Clean(configured), "/")
		if dir != "" && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

func (s Service) Control(ctx context.Context, c cluster.Cluster, action string, out, errOut io.Writer) error {
	switch action {
	case "start", "stop", "restart":
	default:
		return fmt.Errorf("不支持的实例服务操作 %q", action)
	}
	if err := c.Validate(); err != nil {
		return err
	}

	clusters, err := s.Clusters(ctx)
	if err != nil {
		return err
	}
	for _, current := range clusters {
		if current.ID() != c.ID() {
			continue
		}
		if current.Port != c.Port || current.Owner != c.Owner || current.DataDir != c.DataDir {
			return fmt.Errorf("集群 %s 的端口、所有者或数据目录已变更，请重新选择实例", c.ID())
		}
		return s.Runner.Run(ctx, command.Call{
			Name: "/usr/bin/pg_ctlcluster", Args: []string{c.Version, c.Name, action}, AsRoot: true,
			Stdout: out, Stderr: errOut,
		})
	}
	return fmt.Errorf("集群 %s 已不在实例列表中，请重新选择", c.ID())
}

func (s Service) Ready(ctx context.Context, c cluster.Cluster) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	out, err := command.Output(ctx, s.Runner, command.Call{
		Name: "/usr/bin/pg_isready", AsUser: c.Owner,
		Args: []string{"--cluster", c.ID(), "--username=" + c.Owner, "--dbname=postgres", "--timeout=3"},
	})
	return strings.TrimSpace(out), err
}
