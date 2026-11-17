package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/9star-academy/PGReady/internal/archive"
	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
	"github.com/9star-academy/PGReady/internal/host"
	"github.com/9star-academy/PGReady/internal/pg"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, command.Exec{}))
}

func run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, runner command.Runner) int {
	a := app{
		ctx: ctx, in: bufio.NewReader(in), out: out, errOut: errOut,
		host: host.Service{Runner: runner}, pg: pg.Service{Runner: runner},
		archive: archive.Service{Runner: runner},
		style:   menuStyleFor(out),
	}
	if len(args) > 1 {
		fmt.Fprintln(errOut, "用法：pgready [menu|status|--version|--help]")
		return 2
	}
	action := "menu"
	if len(args) == 1 {
		action = args[0]
	}
	var err error
	switch action {
	case "-h", "--help":
		fmt.Fprintln(out, "PGReady · PostgreSQL 管理器")
		fmt.Fprintln(out, "pgready         打开中文数字菜单")
		fmt.Fprintln(out, "pgready status  列出本机 APT 管理的 PostgreSQL 实例")
		fmt.Fprintln(out, "pgready --version  显示当前版本")
		fmt.Fprintln(out, "pgready --help  显示命令行用法")
	case "--version":
		fmt.Fprintln(out, "pgready", version)
	case "status":
		var clusters []cluster.Cluster
		clusters, err = a.host.Clusters(ctx)
		if err == nil {
			a.printClusters(clusters)
		}
	case "menu":
		err = a.mainMenu()
	default:
		fmt.Fprintf(errOut, "未知命令 %q。使用 pgready --help 查看用法。\n", action)
		return 2
	}
	switch {
	case ctx.Err() != nil:
		return 130
	case err == nil || errors.Is(err, io.EOF):
		return 0
	default:
		fmt.Fprintln(errOut, err)
		return 1
	}
}

type app struct {
	ctx      context.Context
	in       *bufio.Reader
	out      io.Writer
	errOut   io.Writer
	host     host.Service
	pg       pg.Service
	archive  archive.Service
	selected *cluster.Cluster
	style    menuStyle
	logoID   uint32
}

type menuItem struct {
	label string
	run   func() error
}

var errCanceled = errors.New("已取消")

func (a *app) line(prompt string) (string, error) {
	fmt.Fprint(a.out, prompt)
	type input struct {
		value string
		err   error
	}
	result := make(chan input, 1)
	go func() {
		line, err := a.in.ReadString('\n')
		if errors.Is(err, io.EOF) && line != "" {
			err = nil
		}
		result <- input{strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), err}
	}()
	select {
	case <-a.ctx.Done():
		return "", a.ctx.Err()
	case next := <-result:
		return next.value, next.err
	}
}

func (a *app) ask(prompt string) (string, error) {
	answer, err := a.line(prompt + "（留空取消）：")
	if err == nil && answer == "" {
		err = errCanceled
	}
	return answer, err
}

func (a *app) confirm(prompt string) error {
	answer, err := a.line(prompt + " [y/N]：")
	if err == nil && !strings.EqualFold(strings.TrimSpace(answer), "y") {
		err = errCanceled
	}
	return err
}

func (a *app) menu(title, zero string, items []menuItem) error {
	for {
		a.menuIntro(title)
		a.menuLine()
		for i, item := range items {
			a.menuItem(i+1, item.label)
		}
		a.menuItem(0, zero)
		a.menuLine()
		answer, err := a.menuChoice()
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(answer)
		if answer == "0" {
			return nil
		}
		var selected func() error
		for i, item := range items {
			if answer == fmt.Sprint(i+1) {
				selected = item.run
				break
			}
		}
		if selected == nil {
			fmt.Fprintln(a.errOut, "无效选项，请重新输入。")
			continue
		}
		err = selected()
		switch {
		case errors.Is(err, io.EOF) || a.ctx.Err() != nil:
			return err
		case errors.Is(err, errCanceled):
			fmt.Fprintln(a.out, err)
		case err != nil:
			fmt.Fprintf(a.errOut, "操作失败：%v\n", err)
		}
	}
}

func (a *app) mainMenu() error {
	clusters, err := a.host.Clusters(a.ctx)
	switch {
	case err != nil:
		fmt.Fprintf(a.errOut, "实例检查：%v\n", err)
	case len(clusters) == 1:
		a.selected = &clusters[0]
	case len(clusters) > 1:
		fmt.Fprintln(a.out, "检测到多个实例，请在 1. 实例与服务 中选择目标。")
	default:
		fmt.Fprintln(a.out, "未发现 APT 管理的实例。新服务器可选择 2. 安装与版本。")
	}
	return a.menu(mainMenuTitle, "退出", []menuItem{
		{"实例与服务", a.instanceMenu},
		{"安装与版本", a.installMenu},
		{"数据库与账号", a.databaseMenu},
		{"备份与恢复", a.archiveMenu},
		{"配置与访问", a.configMenu},
		{"日志与诊断", a.diagnosticMenu},
		{"扩展管理", a.extensionMenu},
		{"帮助与命令速查", a.helpMenu},
	})
}

func (a *app) target() (cluster.Cluster, error) {
	if a.selected == nil {
		return cluster.Cluster{}, fmt.Errorf("请先在 1. 实例与服务 中选择实例")
	}
	clusters, err := a.host.Clusters(a.ctx)
	if err != nil {
		return cluster.Cluster{}, err
	}
	for _, c := range clusters {
		if c.ID() == a.selected.ID() && c.Owner == a.selected.Owner &&
			c.DataDir == a.selected.DataDir && c.Port == a.selected.Port {
			a.selected = &c
			return c, c.Validate()
		}
	}
	a.selected = nil
	return cluster.Cluster{}, fmt.Errorf("原实例已移除或连接信息已改变，请重新选择")
}

func (a *app) onCluster(fn func(cluster.Cluster) error) func() error {
	return func() error {
		c, err := a.target()
		if err != nil {
			return err
		}
		return fn(c)
	}
}

func (a *app) show(result string, err error) error {
	if result != "" {
		fmt.Fprintln(a.out, strings.TrimRight(result, "\n"))
	}
	return err
}

func (a *app) printClusters(clusters []cluster.Cluster) {
	if len(clusters) == 0 {
		fmt.Fprintln(a.out, "未发现 APT 管理的 PostgreSQL 实例。")
	}
	for _, c := range clusters {
		fmt.Fprintf(a.out, "%s  端口 %d  %s  数据目录 %s\n", c.ID(), c.Port, c.Status, c.DataDir)
	}
}
