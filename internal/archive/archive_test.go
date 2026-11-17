package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
)

var testCluster = cluster.Cluster{
	Version: "16", Name: "secondary", Status: "online", Owner: "postgres", Port: 5433,
	DataDir: "/var/lib/postgresql/16/secondary",
}

func TestBackupPublishesPrivateArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.dump")
	service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		assertTarget(t, call, "pg_dump")
		assertArgs(t, call, "--format=custom")
		assertDatabase(t, call, "branch")
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("destination visible before pg_dump finished: %v", err)
		}
		_, err := io.WriteString(call.Stdout, "PGDMP test archive")
		return err
	})}
	if err := service.Backup(context.Background(), testCluster, "branch", path); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "PGDMP test archive")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("permissions = %o, want 0600", info.Mode().Perm())
	}
	assertDirectorySize(t, dir, 1)
}

func TestBackupPreservesExistingFiles(t *testing.T) {
	for _, scenario := range []string{"existing", "concurrent"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "example.dump")
			if scenario == "existing" {
				writeFile(t, path, "first")
			}
			service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
				if scenario == "existing" {
					t.Fatal("pg_dump ran with an existing destination")
				}
				writeFile(t, path, "first")
				_, err := io.WriteString(call.Stdout, "other dump")
				return err
			})}
			if err := service.Backup(context.Background(), testCluster, "branch", path); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("Backup error = %v, want file exists", err)
			}
			assertFile(t, path, "first")
			assertDirectorySize(t, dir, 1)
		})
	}
}

func TestBackupDiscardsFailedOrCanceledDump(t *testing.T) {
	failed := errors.New("pg_dump failed")
	for _, scenario := range []struct {
		name string
		want error
	}{
		{"command failure", failed},
		{"canceled", context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := t.TempDir()
			service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
				if _, err := io.WriteString(call.Stdout, "incomplete"); err != nil {
					return err
				}
				if scenario.want == context.Canceled {
					cancel()
					return nil
				}
				_, _ = io.WriteString(call.Stderr, "database error")
				return failed
			})}
			err := service.Backup(ctx, testCluster, "branch", filepath.Join(dir, "example.dump"))
			if !errors.Is(err, scenario.want) {
				t.Fatalf("Backup error = %v, want %v", err, scenario.want)
			}
			if scenario.want == failed && !strings.Contains(err.Error(), "database error") {
				t.Errorf("Backup error = %v, want PostgreSQL diagnostics", err)
			}
			assertDirectorySize(t, dir, 0)
		})
	}
}

func TestInspectReportsMetadataAndContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "example.dump")
	writeFile(t, path, "archive")
	service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		assertTarget(t, call, "pg_restore")
		assertArgs(t, call, "--list")
		assertInput(t, call, "archive")
		_, err := io.WriteString(call.Stdout, "Table of contents\n")
		return err
	})}
	got, err := service.Inspect(context.Background(), testCluster, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{path, "7 字节", "Table of contents"} {
		if !strings.Contains(got, part) {
			t.Errorf("Inspect output missing %q: %q", part, got)
		}
	}
}

func TestRestoreUsesSameArchiveAndOnlyNewDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "original.dump")
	writeFile(t, path, "original archive")
	calls := 0
	service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		calls++
		switch calls {
		case 1:
			assertTarget(t, call, "pg_restore")
			assertArgs(t, call, "--list")
			assertInput(t, call, "original archive")
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			writeFile(t, path, "archive at replacement path")
		case 2:
			assertTarget(t, call, "psql")
			assertDatabase(t, call, "postgres")
			assertArgs(t, call, "--command=CREATE DATABASE \"db\"\"copy\" TEMPLATE template0;")
		case 3:
			assertTarget(t, call, "pg_restore")
			assertDatabase(t, call, "db\"copy")
			assertArgs(t, call, "--exit-on-error", "--single-transaction")
			assertInput(t, call, "original archive")
			for _, destructive := range []string{"--clean", "--create", "-c", "-C"} {
				if slices.Contains(call.Args, destructive) {
					t.Errorf("unsafe restore argument %s", destructive)
				}
			}
		default:
			t.Fatalf("unexpected call %v", call.Args)
		}
		return nil
	})}
	if err := service.Restore(context.Background(), testCluster, path, "db\"copy", io.Discard); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestRestoreStopsAfterFailureAndReportsProgress(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		failAt  int
		message string
	}{
		{"invalid archive", 1, "读取备份目录"},
		{"database already exists", 2, "创建目标数据库"},
		{"restore interrupted", 3, "新库仍保留"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.dump")
			writeFile(t, path, "sample archive")
			failure := errors.New(scenario.name)
			calls := 0
			service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
				calls++
				if calls == scenario.failAt {
					return failure
				}
				return nil
			})}
			err := service.Restore(context.Background(), testCluster, path, "fresh", io.Discard)
			if !errors.Is(err, failure) || !strings.Contains(err.Error(), scenario.message) {
				t.Fatalf("Restore error = %v, want %q with cause %v", err, scenario.message, failure)
			}
			if calls != scenario.failAt {
				t.Errorf("calls = %d after failed call %d", calls, scenario.failAt)
			}
		})
	}
}

func TestArchiveRejectsInvalidInputsBeforeRunningCommands(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.dump")
	writeFile(t, path, "example")
	service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		t.Fatalf("unexpected command: %s %v", call.Name, call.Args)
		return nil
	})}
	ctx := context.Background()
	for _, result := range []error{
		service.Backup(ctx, testCluster, "", filepath.Join(dir, "new.dump")),
		service.Backup(ctx, testCluster, "branch", ""),
		service.Restore(ctx, testCluster, path, strings.Repeat("x", 64), io.Discard),
		service.Restore(ctx, testCluster, dir, "new_database", io.Discard),
	} {
		if result == nil {
			t.Fatal("invalid input was accepted")
		}
	}
	if _, err := service.Inspect(ctx, testCluster, filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Inspect error = %v, want missing file", err)
	}
	if _, err := service.Verify(ctx, testCluster, "branch", "public", ""); err == nil {
		t.Fatal("Verify accepted an empty table name")
	}
}

func TestVerifyQuotesNamesAndReadsRowCount(t *testing.T) {
	service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		assertTarget(t, call, "psql")
		assertDatabase(t, call, "db_copy")
		assertArgs(t, call, `--command=SELECT pg_catalog.count(*) FROM "my schema"."order""item";`)
		_, err := io.WriteString(call.Stdout, "21\n")
		return err
	})}
	count, err := service.Verify(context.Background(), testCluster, "db_copy", "my schema", `order"item`)
	if err != nil || count != "21" {
		t.Fatalf("Verify = %q, %v, want row count 21", count, err)
	}
}

func TestVerifyRejectsMalformedOutput(t *testing.T) {
	service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		_, err := io.WriteString(call.Stdout, "notice without row count\n")
		return err
	})}
	if _, err := service.Verify(context.Background(), testCluster, "db_copy", "public", "items"); err == nil {
		t.Fatal("Verify accepted malformed row count")
	}
}

func assertTarget(t *testing.T, call command.Call, tool string) {
	t.Helper()
	if filepath.Base(call.Name) != tool || call.AsUser != testCluster.Owner || call.AsRoot {
		t.Fatalf("call = %s with user %q and root=%t, want %s as %s", call.Name, call.AsUser, call.AsRoot, tool, testCluster.Owner)
	}
	if !slices.Contains(call.Args, testCluster.ID()) && !slices.Contains(call.Args, "--cluster="+testCluster.ID()) {
		t.Errorf("args %q do not select %s", call.Args, testCluster.ID())
	}
}

func assertDatabase(t *testing.T, call command.Call, name string) {
	t.Helper()
	assertArgs(t, call, fmt.Sprintf("--dbname=dbname='%s'", name))
}

func assertArgs(t *testing.T, call command.Call, wanted ...string) {
	t.Helper()
	for _, arg := range wanted {
		if !slices.Contains(call.Args, arg) {
			t.Errorf("args %q missing %q", call.Args, arg)
		}
	}
}

func assertInput(t *testing.T, call command.Call, want string) {
	t.Helper()
	if call.Stdin == nil {
		t.Fatal("command has no stdin")
	}
	data, err := io.ReadAll(call.Stdin)
	if err != nil || string(data) != want {
		t.Fatalf("stdin = %q, %v, want %q", data, err, want)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("file = %q, %v, want %q", got, err, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertDirectorySize(t *testing.T, dir string, want int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Errorf("directory entries = %v, want %d entries", entries, want)
	}
}
