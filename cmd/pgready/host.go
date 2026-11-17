package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/host"
)

func (a *app) instanceMenu() error {
	return a.menu("实例与服务", "返回主菜单", []menuItem{
		{"列出与切换实例", a.selectInstance},
		{"查看当前实例", a.onCluster(func(c cluster.Cluster) error {
			a.printClusters([]cluster.Cluster{c})
			return a.show(a.host.Ready(a.ctx, c))
		})},
		{"启动实例", a.onCluster(a.controlInstance("start", "启动"))},
		{"停止实例", a.onCluster(a.controlInstance("stop", "停止，会中断现有连接"))},
		{"重启实例", a.onCluster(a.controlInstance("restart", "重启，会中断现有连接"))},
	})
}

func (a *app) selectInstance() error {
	clusters, err := a.host.Clusters(a.ctx)
	if err != nil {
		return err
	}
	if len(clusters) == 0 {
		a.printClusters(clusters)
		return nil
	}
	for {
		fmt.Fprintf(a.out, "\n%s\n", a.style.title("选择 PostgreSQL 实例"))
		a.menuLine()
		for i, c := range clusters {
			a.menuItem(i+1, fmt.Sprintf("%s  端口 %d  %s  数据目录 %s", c.ID(), c.Port, a.style.status(c.Status), c.DataDir))
		}
		a.menuItem(0, "返回")
		a.menuLine()
		answer, err := a.menuChoice()
		if err != nil {
			return err
		}
		if answer == "0" {
			return nil
		}
		for i, c := range clusters {
			if answer == fmt.Sprint(i+1) {
				if err := c.Validate(); err != nil {
					return err
				}
				a.selected = &c
				fmt.Fprintf(a.out, "已选择 %s。\n", c.ID())
				return nil
			}
		}
		fmt.Fprintln(a.errOut, "无效选项，请重新输入。")
	}
}

func (a *app) controlInstance(action, label string) func(cluster.Cluster) error {
	return func(c cluster.Cluster) error {
		if err := a.confirm(fmt.Sprintf("集群 %s，端口 %d，数据目录 %q。确认%s？", c.ID(), c.Port, c.DataDir, label)); err != nil {
			return err
		}
		controlErr := a.host.Control(a.ctx, c, action, a.out, a.errOut)
		_, refreshErr := a.target()
		if controlErr != nil {
			return controlErr
		}
		if refreshErr != nil {
			return fmt.Errorf("服务命令已执行，刷新实例状态失败: %w", refreshErr)
		}
		a.printClusters([]cluster.Cluster{*a.selected})
		if action != "stop" {
			return a.show(a.host.Ready(a.ctx, *a.selected))
		}
		return nil
	}
}

func (a *app) installMenu() error {
	return a.menu("安装与版本", "返回主菜单", []menuItem{
		{"检查安装环境", func() error { return a.show(a.host.Environment(a.ctx)) }},
		{"查看可安装版本", a.listPackages},
		{"安装 PostgreSQL", a.installPostgres},
		{"检查补丁更新", a.onCluster(func(c cluster.Cluster) error {
			pkg, err := a.host.Package(a.ctx, c.Version)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.out, "PostgreSQL %s  已安装 %s  候选 %s\n", pkg.Major, pkg.Installed, pkg.Candidate)
			return nil
		})},
	})
}

func (a *app) listPackages() error {
	packages, err := a.installPackages()
	if err != nil {
		return err
	}
	for _, pkg := range packages {
		source := "本机 APT 源"
		if pkg.PGDG {
			source = "官方 PGDG 源"
		}
		fmt.Fprintf(a.out, "PostgreSQL %s  已安装 %s  候选 %s  %s\n", pkg.Major, pkg.Installed, pkg.Candidate, source)
	}
	return nil
}

func (a *app) installPackages() ([]host.Package, error) {
	if time.Now().Before(a.installCandidatesUntil) {
		return a.installCandidates, nil
	}
	fmt.Fprintln(a.out, "正在查询 PostgreSQL 软件源…")
	packages, err := a.host.InstallPackages(a.ctx)
	if err != nil && len(packages) != 0 {
		fmt.Fprintf(a.errOut, "%v\n当前仅显示本机 APT 源中的可安装版本。\n", err)
		return packages, nil
	}
	if err == nil {
		a.installCandidates = packages
		a.installCandidatesUntil = time.Now().Add(time.Minute)
	}
	return packages, err
}

func (a *app) installPostgres() error {
	packages, err := a.installPackages()
	if err != nil {
		return err
	}
	selected, err := a.chooseInstallPackage(packages)
	if err != nil {
		return err
	}
	a.installCandidatesUntil = time.Time{}
	if err := a.host.Install(a.ctx, selected, a.out, a.errOut); err != nil {
		return err
	}
	clusters, err := a.host.Clusters(a.ctx)
	if err != nil {
		return fmt.Errorf("APT 安装后无法读取集群: %w", err)
	}
	a.printClusters(clusters)
	if len(clusters) == 1 {
		a.selected = &clusters[0]
		return a.show(a.host.Ready(a.ctx, clusters[0]))
	}
	return nil
}

func (a *app) chooseInstallPackage(packages []host.Package) (host.Package, error) {
	a.menuIntro("选择 PostgreSQL 版本")
	a.menuLine()
	for i, pkg := range packages {
		a.menuItem(i+1, fmt.Sprintf("PostgreSQL %s（%s）", pkg.Major, pkg.Candidate))
	}
	a.menuItem(0, "返回")
	a.menuLine()
	for {
		answer, err := a.menuChoice()
		if err != nil {
			return host.Package{}, err
		}
		choice, err := strconv.Atoi(strings.TrimSpace(answer))
		if err != nil || choice < 0 || choice > len(packages) {
			fmt.Fprintln(a.errOut, "无效选项，请重新输入。")
			continue
		}
		if choice == 0 {
			return host.Package{}, errCanceled
		}
		pkg := packages[choice-1]
		if pkg.PGDG {
			fmt.Fprintln(a.out, "使用 PostgreSQL PGDG 软件源。确认后将按需配置该源，以便通过 APT 更新。")
		}
		if err := a.confirm(fmt.Sprintf("确认通过 APT 安装 PostgreSQL %s（%s）？", pkg.Major, pkg.Candidate)); err != nil {
			return host.Package{}, err
		}
		return pkg, nil
	}
}
