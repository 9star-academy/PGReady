package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
)

func (a *app) databaseMenu() error {
	return a.menu("数据库与账号", "返回主菜单", []menuItem{
		{"列出数据库", a.onCluster(func(c cluster.Cluster) error {
			return a.show(a.pg.ListDatabases(a.ctx, c))
		})},
		{"创建数据库", a.onCluster(a.createDatabase)},
		{"列出账号与角色", a.onCluster(func(c cluster.Cluster) error {
			return a.show(a.pg.ListRoles(a.ctx, c))
		})},
		{"创建应用账号", a.onCluster(a.createAccount)},
		{"调整密码与权限", a.permissionsMenu},
		{"测试应用账号登录", a.onCluster(a.testLogin)},
	})
}

func (a *app) createDatabase(c cluster.Cluster) error {
	name, err := a.ask("新数据库名")
	if err != nil {
		return err
	}
	owner, err := a.line("数据库所有者（留空使用实例默认管理员）：")
	if err != nil {
		return err
	}
	if err := a.confirmDatabaseAction(c, fmt.Sprintf("创建数据库 %q", name)); err != nil {
		return err
	}
	if err := a.pg.CreateDatabase(a.ctx, c, name, owner); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "已创建数据库 %q。\n", name)
	return nil
}

func (a *app) createAccount(c cluster.Cluster) error {
	db, account, err := a.databaseAndAccount()
	if err != nil {
		return err
	}
	if err := a.confirmDatabaseAction(c, fmt.Sprintf("创建应用账号 %q 并为数据库 %q 授予 CONNECT", account, db)); err != nil {
		return err
	}
	if err := a.pg.CreateAccount(a.ctx, c, db, account, a.out, a.errOut); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "已创建应用账号 %q 并授予对 %q 的 CONNECT 权限。表和模式权限按业务需要分配。\n", account, db)
	return nil
}

func (a *app) testLogin(c cluster.Cluster) error {
	db, account, err := a.databaseAndAccount()
	if err != nil {
		return err
	}
	if err := a.verifyDatabaseTarget(c); err != nil {
		return err
	}
	return a.pg.TestLogin(a.ctx, c, db, account, a.out, a.errOut)
}

func (a *app) databaseAndAccount() (string, string, error) {
	db, err := a.ask("数据库名")
	if err != nil {
		return "", "", err
	}
	account, err := a.ask("账号名")
	return db, account, err
}

func (a *app) permissionsMenu() error {
	return a.menu("调整密码与权限", "返回数据库与账号", []menuItem{
		{"查看数据库 CONNECT 权限", a.onCluster(func(c cluster.Cluster) error {
			db, err := a.ask("数据库名")
			if err != nil {
				return err
			}
			return a.show(a.pg.ConnectPrivileges(a.ctx, c, db))
		})},
		{"修改登录密码", a.onCluster(func(c cluster.Cluster) error {
			account, err := a.ask("账号名")
			if err != nil {
				return err
			}
			if err := a.confirmDatabaseAction(c, fmt.Sprintf("修改账号 %q 的密码", account)); err != nil {
				return err
			}
			return a.pg.ChangePassword(a.ctx, c, account, a.out, a.errOut)
		})},
		{"授予显式 CONNECT 权限", a.onCluster(a.changeConnect("授予", true))},
		{"撤销显式 CONNECT 权限", a.onCluster(a.changeConnect("撤销", false))},
	})
}

func (a *app) changeConnect(label string, grant bool) func(cluster.Cluster) error {
	return func(c cluster.Cluster) error {
		db, account, err := a.databaseAndAccount()
		if err != nil {
			return err
		}
		if err := a.confirmDatabaseAction(c, fmt.Sprintf("%s账号 %q 对数据库 %q 的显式 CONNECT 权限", label, account, db)); err != nil {
			return err
		}
		if err := a.pg.SetConnect(a.ctx, c, db, account, grant); err != nil {
			return err
		}
		fmt.Fprintln(a.out, "已更新显式 CONNECT 权限。账号的实际访问也受 PUBLIC 与角色继承影响。")
		return nil
	}
}

func (a *app) confirmDatabaseAction(c cluster.Cluster, operation string) error {
	if err := a.confirm(fmt.Sprintf("集群 %s，端口 %d。确认%s？", c.ID(), c.Port, operation)); err != nil {
		return err
	}
	return a.verifyDatabaseTarget(c)
}

func (a *app) verifyDatabaseTarget(c cluster.Cluster) error {
	if !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("集群 %s 缺少可核对的数据目录", c.ID())
	}
	call, err := c.Client("psql", "postgres", "-X", "-At", "-v", "ON_ERROR_STOP=1", "-c", "SHOW data_directory")
	if err != nil {
		return err
	}
	dir, err := command.Output(a.ctx, a.pg.Runner, call)
	if err != nil {
		return fmt.Errorf("核对集群 %s 的数据库连接失败: %w", c.ID(), err)
	}
	if filepath.Clean(strings.TrimSuffix(dir, "\n")) != filepath.Clean(c.DataDir) {
		return fmt.Errorf("已连接的数据库数据目录与集群 %s 的记录不符，请重新选择实例", c.ID())
	}
	return nil
}

func (a *app) configMenu() error {
	return a.menu("配置与访问", "返回主菜单", []menuItem{
		{"查看目录与配置文件", a.config("paths")},
		{"查看监听与端口", a.config("listen")},
		{"查看客户端认证", a.config("hba")},
		{"查看生效参数", a.config("settings")},
	})
}

func (a *app) config(view string) func() error {
	return a.onCluster(func(c cluster.Cluster) error {
		return a.show(a.pg.Config(a.ctx, c, view))
	})
}

func (a *app) diagnosticMenu() error {
	return a.menu("日志与诊断", "返回主菜单", []menuItem{
		{"查看服务日志", a.diagnostic("logs")},
		{"查看慢查询日志", a.diagnostic("slow")},
		{"查看连接与活动", a.diagnostic("connections")},
		{"查看锁与阻塞", a.diagnostic("locks")},
		{"检查存储空间", a.diagnostic("disk")},
	})
}

func (a *app) diagnostic(view string) func() error {
	return a.onCluster(func(c cluster.Cluster) error {
		return a.show(a.pg.Diagnostics(a.ctx, c, view))
	})
}

func (a *app) extensionMenu() error {
	return a.menu("扩展管理", "返回主菜单", []menuItem{
		{"查看可用扩展", a.onCluster(func(c cluster.Cluster) error {
			return a.show(a.pg.Extensions(a.ctx, c, "postgres", false))
		})},
		{"查看已启用扩展", a.onCluster(func(c cluster.Cluster) error {
			db, err := a.ask("数据库名")
			if err != nil {
				return err
			}
			return a.show(a.pg.Extensions(a.ctx, c, db, true))
		})},
	})
}
