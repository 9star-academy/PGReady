package main

import "fmt"

func (a *app) helpMenu() error {
	return a.menu("帮助与命令速查", "返回主菜单", []menuItem{
		{"快速上手", a.quickStart},
		{"常用命令", a.commands},
		{"常见问题", a.troubleshoot},
	})
}

func (a *app) quickStart() error {
	port := 5432
	if a.selected != nil {
		port = a.selected.Port
	} else {
		fmt.Fprintln(a.out, "下面用默认端口 5432 举例，可以在「实例与服务」中切换实例。")
	}
	fmt.Fprintln(a.out, "在 SSH 终端输入以下命令，替换 app_user 和 app_db。示例使用本机 IPv4 连接。")
	fmt.Fprintf(a.out, "psql -h 127.0.0.1 -p %d -U app_user -d app_db\n", port)
	fmt.Fprintln(a.out, `进入 psql 后输入 \conninfo 核对连接，输入 \q 退出。`)
	return nil
}

func (a *app) commands() error {
	fmt.Fprintln(a.out, `在 SSH 终端输入
  pgready         打开数字菜单
  pgready status  查看本机 PostgreSQL 实例
  pgready update  将 PGReady 更新到最新正式版

进入 psql 后输入
  \conninfo       查看当前连接
  \l              列出数据库
  \dt             列出搜索路径下可见的表
  \d 表名         查看表结构
  \du             列出角色
  \q              退出 psql`)
	return nil
}

func (a *app) troubleshoot() error {
	fmt.Fprintln(a.out, `连接被拒绝    用「实例与服务」和「配置与访问」检查服务、监听地址和端口。
密码认证失败  核对账号、库名和客户端认证规则，再用「数据库与账号」测试登录。
权限不足      查看数据库的 CONNECT 权限，并核对角色继承和表权限。
服务起不来    用「日志与诊断」查看服务日志和数据目录的可用空间。`)
	return nil
}
