package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
)

type Service struct {
	Runner command.Runner
}

func (s Service) Backup(ctx context.Context, c cluster.Cluster, db, path string) error {
	if _, err := cluster.QuoteIdentifier(db); err != nil {
		return fmt.Errorf("数据库名无效: %w", err)
	}
	if strings.TrimSpace(path) == "" {
		return errors.New("请输入备份文件路径")
	}
	call, err := c.Client("pg_dump", db, "--format=custom")
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("备份目标 %q: %w", path, fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查备份目标 %q: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pgready-backup-*")
	if err != nil {
		return fmt.Errorf("在目标目录创建临时备份: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	call.Stdout = tmp
	if err := s.run(ctx, call); err != nil {
		return fmt.Errorf("备份实例 %s 的数据库 %q: %w", c.ID(), db, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("保存临时备份: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时备份: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A hard link publishes the completed archive without replacing a file
	// created by another process after the initial check.
	if err := os.Link(tmp.Name(), path); err != nil {
		return fmt.Errorf("保存备份到 %q: %w", path, err)
	}
	return nil
}

func (s Service) Inspect(ctx context.Context, c cluster.Cluster, path string) (string, error) {
	file, info, err := openArchive(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	call, err := c.Client("pg_restore", "", "--list")
	if err != nil {
		return "", err
	}
	call.Stdin = file
	output, err := command.Output(ctx, s.Runner, call)
	if err != nil {
		return "", fmt.Errorf("读取备份目录 %q: %w", path, err)
	}
	return fmt.Sprintf("文件 %s\n大小 %d 字节\n修改时间 %s\n\n%s", path, info.Size(),
		info.ModTime().Format(time.RFC3339), output), nil
}

func (s Service) Restore(ctx context.Context, c cluster.Cluster, path, newDatabase string, errOut io.Writer) error {
	name, err := cluster.QuoteIdentifier(newDatabase)
	if err != nil {
		return fmt.Errorf("新数据库名无效: %w", err)
	}
	list, err := c.Client("pg_restore", "", "--list")
	if err != nil {
		return err
	}
	create, err := c.Client("psql", "postgres", "-X", "--set=ON_ERROR_STOP=1",
		"--command=CREATE DATABASE "+name+" TEMPLATE template0;")
	if err != nil {
		return err
	}
	restore, err := c.Client("pg_restore", newDatabase, "--exit-on-error", "--single-transaction")
	if err != nil {
		return err
	}
	file, _, err := openArchive(path)
	if err != nil {
		return err
	}
	defer file.Close()

	list.Stdin = file
	list.Stdout = io.Discard
	if err := s.run(ctx, list); err != nil {
		return fmt.Errorf("读取备份目录 %q: %w", path, err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("重新读取备份 %q: %w", path, err)
	}
	// CREATE DATABASE rejects a conflicting name atomically on the server.
	create.Stdout = io.Discard
	if err := s.run(ctx, create); err != nil {
		return fmt.Errorf("在实例 %s 创建目标数据库 %q: %w", c.ID(), newDatabase, err)
	}
	restore.Stdin = file
	restore.Stdout = io.Discard
	restore.Stderr = errOut
	if err := s.run(ctx, restore); err != nil {
		return fmt.Errorf("恢复到实例 %s 的数据库 %q 失败，新库仍保留。请检查失败原因，重试时使用另一个新库名: %w",
			c.ID(), newDatabase, err)
	}
	return nil
}

func (s Service) Verify(ctx context.Context, c cluster.Cluster, db, schema, table string) (string, error) {
	if _, err := cluster.QuoteIdentifier(db); err != nil {
		return "", fmt.Errorf("数据库名无效: %w", err)
	}
	schemaName, err := cluster.QuoteIdentifier(schema)
	if err != nil {
		return "", fmt.Errorf("模式名无效: %w", err)
	}
	tableName, err := cluster.QuoteIdentifier(table)
	if err != nil {
		return "", fmt.Errorf("表名无效: %w", err)
	}
	query := fmt.Sprintf("SELECT pg_catalog.count(*) FROM %s.%s;", schemaName, tableName)
	call, err := c.Client("psql", db, "-X", "--set=ON_ERROR_STOP=1", "--no-align", "--tuples-only", "--command="+query)
	if err != nil {
		return "", err
	}
	output, err := command.Output(ctx, s.Runner, call)
	if err != nil {
		return "", fmt.Errorf("验证数据库 %q 的 %s.%s: %w", db, schemaName, tableName, err)
	}
	count := strings.TrimSpace(output)
	if n, err := strconv.ParseUint(count, 10, 64); err != nil || n > (1<<63)-1 {
		return "", fmt.Errorf("读取表 %s.%s 的行数失败，结果为 %q", schemaName, tableName, count)
	}
	return count, nil
}

func openArchive(path string) (*os.File, fs.FileInfo, error) {
	// The caller opens the archive before changing the client process identity.
	// O_NONBLOCK prevents a FIFO at the supplied path from blocking the open.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("打开备份 %q: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, fmt.Errorf("检查备份 %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, fmt.Errorf("备份 %q 必须是普通文件", path)
	}
	return file, info, nil
}

func (s Service) run(ctx context.Context, call command.Call) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var diagnostics bytes.Buffer
	if call.Stderr == nil {
		call.Stderr = &diagnostics
	}
	if err := s.Runner.Run(ctx, call); err != nil {
		if detail := strings.TrimSpace(diagnostics.String()); detail != "" {
			return fmt.Errorf("%w\n%s", err, detail)
		}
		return err
	}
	return nil
}
