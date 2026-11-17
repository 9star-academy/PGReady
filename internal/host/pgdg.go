package host

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/9star-academy/PGReady/internal/command"
)

const (
	pgdgRepository      = "apt.postgresql.org/pub/repos/apt"
	pgdgChinaRepository = "mirrors.nju.edu.cn/postgresql/repos/apt"
)

// PGDG archive signing key B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8.
//
//go:embed pgdg.asc
var pgdgKey string

var stableUpstreamVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+(\+dfsg[0-9]*)?$`)

func stablePackageVersion(pkg Package) bool {
	version := pkg.Candidate
	if _, upstream, ok := strings.Cut(version, ":"); ok {
		version = upstream
	}
	if revision := strings.LastIndex(version, "-"); revision >= 0 {
		version = version[:revision]
	}
	return strings.HasPrefix(version, pkg.Major+".") && stableUpstreamVersion.MatchString(version)
}

func aptCommand(config, name string, args ...string) command.Call {
	if config != "" {
		return command.Call{Name: "/usr/bin/env", Args: append([]string{"APT_CONFIG=" + config, name}, args...)}
	}
	return command.Call{Name: name, Args: args}
}

// InstallPackages returns official candidates and local-only versions. A partial
// list and an error mean that PGDG could not be queried and only local packages are shown.
func (s Service) InstallPackages(ctx context.Context) ([]Package, error) {
	release, err := s.operatingSystem()
	if err != nil {
		return nil, err
	}
	local, localErr := s.aptPackages(ctx, "")
	official, officialErr := s.previewPGDG(ctx, release.codename)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	byMajor := make(map[string]Package)
	for _, pkg := range local {
		if stablePackageVersion(pkg) {
			byMajor[pkg.Major] = pkg
		}
	}
	for _, pkg := range official {
		if stablePackageVersion(pkg) {
			pkg.PGDG = true
			pkg.Installed = byMajor[pkg.Major].Installed
			byMajor[pkg.Major] = pkg
		}
	}
	packages := make([]Package, 0, len(byMajor))
	for _, pkg := range byMajor {
		packages = append(packages, pkg)
	}
	sort.Slice(packages, func(i, j int) bool {
		aMajor, aMinor, _ := strings.Cut(packages[i].Major, ".")
		bMajor, bMinor, _ := strings.Cut(packages[j].Major, ".")
		if len(aMajor) != len(bMajor) {
			return len(aMajor) > len(bMajor)
		}
		if aMajor != bMajor {
			return aMajor > bMajor
		}
		if len(aMinor) != len(bMinor) {
			return len(aMinor) > len(bMinor)
		}
		return aMinor > bMinor
	})
	if officialErr != nil {
		return packages, errors.Join(fmt.Errorf("查询 PostgreSQL 官方 PGDG 源: %w", officialErr), localErr)
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("未找到可安装的 PostgreSQL 正式版本")
	}
	return packages, nil
}

func (s Service) pgdgSource(ctx context.Context, codename, repository string) (string, error) {
	out, err := command.Output(ctx, s.Runner, command.Call{Name: "/usr/bin/dpkg", Args: []string{"--print-architecture"}})
	if err != nil {
		return "", fmt.Errorf("读取 APT 架构: %w", err)
	}
	arch := strings.TrimSpace(out)
	switch arch {
	case "amd64", "arm64", "ppc64el":
	default:
		return "", fmt.Errorf("PGDG 暂未配置架构 %q", arch)
	}
	var source strings.Builder
	fmt.Fprintf(&source, "Types: deb\nURIs: https://%s\nSuites: %s-pgdg\nArchitectures: %s\nComponents: main\nSigned-By:\n", repository, codename, arch)
	for _, line := range strings.Split(strings.TrimSpace(pgdgKey), "\n") {
		if line == "" {
			line = "."
		}
		fmt.Fprintf(&source, " %s\n", line)
	}
	return source.String(), nil
}

func (s Service) previewPGDG(ctx context.Context, codename string) ([]Package, error) {
	repositories := []string{pgdgRepository, pgdgChinaRepository}
	switch os.Getenv("PGREADY_DOWNLOAD_MODE") {
	case "cn":
		repositories[0], repositories[1] = repositories[1], repositories[0]
	case "direct":
		repositories = repositories[:1]
	}
	var failures []error
	for _, repository := range repositories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		packages, err := s.previewPGDGRepository(ctx, codename, repository)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", repository, err))
			continue
		}
		for i := range packages {
			packages[i].pgdgRepository = repository
		}
		return packages, nil
	}
	return nil, errors.Join(failures...)
}

func (s Service) previewPGDGRepository(ctx context.Context, codename, repository string) ([]Package, error) {
	ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	source, err := s.pgdgSource(ctx, codename, repository)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("/tmp", "pgready-pgdg-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	// When launched as root, APT uses _apt to download and verify the index.
	if err := os.Chmod(dir, 0755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "lists", "partial"), 0755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "pgdg.sources"), []byte(source), 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), nil, 0644); err != nil {
		return nil, err
	}
	// APT_CONFIG is read before system configuration, so system update hooks are not loaded.
	config := fmt.Sprintf(`Dir::Etc::parts %q;
Dir::Etc::main "/dev/null";
Dir::Etc::sourcelist %q;
Dir::Etc::sourceparts %q;
Dir::Etc::preferences "/dev/null";
Dir::Etc::preferencesparts %q;
Dir::State::lists %q;
Dir::State::status %q;
Dir::Cache::pkgcache "";
Dir::Cache::srcpkgcache "";
Acquire::Languages "none";
Acquire::Retries "0";
APT::Update::Error-Mode "any";
`, filepath.Join(dir, "empty"), filepath.Join(dir, "pgdg.sources"), filepath.Join(dir, "empty"),
		filepath.Join(dir, "empty"), filepath.Join(dir, "lists"), filepath.Join(dir, "status"))
	configPath := filepath.Join(dir, "apt.conf")
	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		return nil, err
	}
	if _, err := command.Output(ctx, s.Runner, aptCommand(configPath, "/usr/bin/apt-get", "update")); err != nil {
		return nil, err
	}
	return s.aptPackages(ctx, configPath)
}

func (s Service) enablePGDG(ctx context.Context, codename, repository string, out, errOut io.Writer) error {
	if repository == "" {
		repository = pgdgRepository
	}
	targets, err := command.Output(ctx, s.Runner, command.Call{Name: "/usr/bin/apt-get", Args: []string{
		"indextargets", "--no-release-info", "--format", "$(REPO_URI) $(RELEASE)",
	}})
	if err != nil {
		return fmt.Errorf("检查现有 APT 软件源: %w", err)
	}
	enabled := false
	for _, line := range strings.Split(targets, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == codename+"-pgdg" {
			uri := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(fields[0], "https://"), "http://"), "/")
			if uri == repository {
				enabled = true
				break
			}
		}
	}
	if !enabled {
		source, err := s.pgdgSource(ctx, codename, repository)
		if err != nil {
			return err
		}
		// Install atomically without replacing an administrator's existing file.
		const writeSource = `set -eu
file=$(mktemp /etc/apt/sources.list.d/.pgready-pgdg.XXXXXXXX)
trap 'rm -f -- "$file"' EXIT
cat > "$file"
chmod 0644 "$file"
ln -T -- "$file" /etc/apt/sources.list.d/pgready-pgdg.sources`
		if _, err := command.Output(ctx, s.Runner, command.Call{
			Name: "/bin/sh", Args: []string{"-c", writeSource}, AsRoot: true, Stdin: strings.NewReader(source),
		}); err != nil {
			return fmt.Errorf("添加 PGDG APT 软件源: %w", err)
		}
	}
	if err := s.Runner.Run(ctx, command.Call{Name: "/usr/bin/apt-get", Args: []string{"update", "-o", "APT::Update::Error-Mode=any"},
		AsRoot: true, Stdout: out, Stderr: errOut}); err != nil {
		return fmt.Errorf("刷新 APT 软件源: %w", err)
	}
	return nil
}
