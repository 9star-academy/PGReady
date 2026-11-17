package main

import (
	"fmt"

	"github.com/9star-academy/PGReady/internal/cluster"
)

func (a *app) archiveMenu() error {
	return a.menu("备份与恢复", "返回主菜单", []menuItem{
		{"备份指定数据库", a.onCluster(a.backupDatabase)},
		{"查看备份", a.onCluster(a.inspectArchive)},
		{"恢复到新库", a.onCluster(a.restoreDatabase)},
		{"验证恢复结果", a.onCluster(a.verifyRestore)},
	})
}

func (a *app) backupDatabase(c cluster.Cluster) error {
	db, err := a.ask("要备份的数据库名")
	if err != nil {
		return err
	}
	path, err := a.ask("备份保存路径")
	if err != nil {
		return err
	}
	if err := a.confirmDatabaseAction(c, fmt.Sprintf("备份数据库 %q 到 %q", db, path)); err != nil {
		return err
	}
	if err := a.archive.Backup(a.ctx, c, db, path); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "备份已保存到 %q。可通过「恢复到新库」进行演练。\n", path)
	return nil
}

func (a *app) inspectArchive(c cluster.Cluster) error {
	path, err := a.ask("本地备份文件路径")
	if err != nil {
		return err
	}
	return a.show(a.archive.Inspect(a.ctx, c, path))
}

func (a *app) restoreDatabase(c cluster.Cluster) error {
	path, err := a.ask("备份文件路径")
	if err != nil {
		return err
	}
	db, err := a.ask("新数据库名")
	if err != nil {
		return err
	}
	fmt.Fprintln(a.out, "数据库恢复会执行归档中的 SQL，仅恢复来自可信服务器的备份。")
	if err := a.confirmDatabaseAction(c, fmt.Sprintf("从 %q 恢复到新数据库 %q", path, db)); err != nil {
		return err
	}
	if err := a.archive.Restore(a.ctx, c, path, db, a.errOut); err != nil {
		return err
	}
	fmt.Fprintf(a.out, "恢复到 %q 完成。请用「验证恢复结果」检查关键表。\n", db)
	return nil
}

func (a *app) verifyRestore(c cluster.Cluster) error {
	db, err := a.ask("恢复后的数据库名")
	if err != nil {
		return err
	}
	schema, err := a.line("模式名（默认 public）：")
	if err != nil {
		return err
	}
	if schema == "" {
		schema = "public"
	}
	table, err := a.ask("关键表名")
	if err != nil {
		return err
	}
	rows, err := a.archive.Verify(a.ctx, c, db, schema, table)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "表 %q.%q 可查询，共 %s 行。请与预期数据核对。\n", schema, table, rows)
	return nil
}
