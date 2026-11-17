package pg

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
)

func TestSelectedReadOnlyQueries(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name, fragment string
		run            func(Service) (string, error)
	}{
		{"databases", "pg_catalog.pg_database", func(s Service) (string, error) { return s.ListDatabases(ctx, testCluster) }},
		{"roles", "pg_catalog.pg_auth_members", func(s Service) (string, error) { return s.ListRoles(ctx, testCluster) }},
		{"connect ACL", "pg_catalog.aclexplode", func(s Service) (string, error) { return s.ConnectPrivileges(ctx, testCluster, "sales") }},
		{"configuration", "pending_restart", func(s Service) (string, error) { return s.Config(ctx, testCluster, "settings") }},
		{"paths", "config_file", func(s Service) (string, error) { return s.Config(ctx, testCluster, "paths") }},
		{"listen", "unix_socket_directories", func(s Service) (string, error) { return s.Config(ctx, testCluster, "listen") }},
		{"authentication", "pg_catalog.pg_hba_file_rules", func(s Service) (string, error) { return s.Config(ctx, testCluster, "hba") }},
		{"connection counts", "pg_catalog.pg_stat_activity", func(s Service) (string, error) { return s.Diagnostics(ctx, testCluster, "connections") }},
		{"blocked", "pg_catalog.pg_blocking_pids", func(s Service) (string, error) { return s.Diagnostics(ctx, testCluster, "locks") }},
		{"available extensions", "pg_catalog.pg_available_extensions", func(s Service) (string, error) { return s.Extensions(ctx, testCluster, "", false) }},
		{"enabled extensions", "pg_catalog.pg_extension", func(s Service) (string, error) { return s.Extensions(ctx, testCluster, "reporting", true) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
				verifyAdmin(t, call)
				args := strings.Join(call.Args, " ")
				if !strings.Contains(args, tc.fragment) || strings.Contains(args, "ldapbindpasswd") {
					t.Fatalf("unexpected query %q", call.Args)
				}
				_, err := fmt.Fprintln(call.Stdout, "result")
				return err
			})}
			output, err := tc.run(svc)
			if err != nil || !strings.Contains(output, "result") {
				t.Fatalf("query got %v, output %q", err, output)
			}
		})
	}
}

func TestRevokeConnectChangesOnlyNamedRole(t *testing.T) {
	svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		verifyAdmin(t, call)
		want := `--command=REVOKE CONNECT ON DATABASE "sales" FROM "reader";`
		if !slices.Contains(call.Args, want) {
			t.Fatalf("expected %q, got %q", want, call.Args)
		}
		return nil
	})}
	if err := svc.SetConnect(context.Background(), testCluster, "sales", "reader", false); err != nil {
		t.Fatal(err)
	}
}

func TestSlowQueryReadsOnlyRecentLogAtSelectedThreshold(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "server log.txt")
	if err := os.WriteFile(logPath, nil, 0600); err != nil {
		t.Fatal(err)
	}

	target := testCluster
	target.LogFile = "pg_log/postgresql.log"
	svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		switch {
		case call.Name == "/usr/bin/tail":
			if call.AsRoot || call.AsUser != "" || !slices.Equal(call.Args, []string{"--bytes=65536", "--", logPath}) {
				t.Fatalf("unexpected log call: %#v", call)
			}
			_, err := fmt.Fprintln(call.Stdout,
				"2026 LOG:  duration: 11.000 ms  statement: SELECT fast\n"+
					"2026 LOG:  duration: 90.000 ms  statement: SELECT slow\n"+
					"2026 LOG:  statement: SELECT 'duration: 900.000 ms'")
			return err
		case strings.Contains(strings.Join(call.Args, " "), "FROM thresholds"):
			_, err := fmt.Fprintln(call.Stdout, "100,50,0.1,50")
			return err
		case strings.Contains(strings.Join(call.Args, " "), "pg_current_logfile"):
			writer := csv.NewWriter(call.Stdout)
			if err := writer.Write([]string{filepath.Base(logPath), filepath.Dir(logPath)}); err != nil {
				return err
			}
			writer.Flush()
			return writer.Error()
		default:
			t.Fatalf("unexpected call: %#v", call)
			return nil
		}
	})}

	out, err := svc.Diagnostics(context.Background(), target, "slow")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SELECT slow") || strings.Contains(out, "SELECT fast") || strings.Contains(out, "900.000") {
		t.Fatalf("unexpected recent slow log %q", out)
	}
}

func TestInactiveSlowLoggingDoesNotReadFile(t *testing.T) {
	svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		verifyAdmin(t, call)
		_, err := fmt.Fprintln(call.Stdout, "-1,-1,1,-1")
		return err
	})}
	out, err := svc.Diagnostics(context.Background(), testCluster, "slow")
	if err != nil || !strings.Contains(out, "未启用") {
		t.Fatalf("diagnostic got %v, output %q", err, out)
	}
}

func TestDiskUsesSelectedDataDirectory(t *testing.T) {
	svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		if call.Name == "/usr/bin/df" {
			if call.AsUser != testCluster.Owner || !slices.Equal(call.Args, []string{"-hP", "--", testCluster.DataDir}) {
				t.Fatalf("unexpected filesystem probe %#v", call)
			}
			_, err := io.WriteString(call.Stdout, "disk")
			return err
		}
		verifyAdmin(t, call)
		_, err := io.WriteString(call.Stdout, "databases")
		return err
	})}

	out, err := svc.Diagnostics(context.Background(), testCluster, "disk")
	if err != nil || out != "databases\ndisk" {
		t.Fatalf("disk diagnostic got %v, output %q", err, out)
	}
}

func TestLogStopsForUnresolvedPath(t *testing.T) {
	svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		t.Fatalf("unexpected call: %#v", call)
		return nil
	})}
	if _, err := svc.Diagnostics(context.Background(), cluster.Cluster{
		Version: "17", Name: "reporting", Status: "down", Owner: "postgres", Port: 5442, LogFile: "syslog",
	}, "logs"); err == nil {
		t.Fatal("expected an error for a log destination that does not identify a regular file")
	}
}
