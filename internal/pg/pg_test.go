package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
)

var testCluster = cluster.Cluster{
	Version: "17", Name: "reporting", Status: "online", Owner: "postgres",
	Port: 5442, DataDir: "/var/lib/postgresql/17/reporting", LogFile: "/var/log/postgresql/postgresql-17-reporting.log",
}

func TestCreateDatabaseQuotesBothIdentifiers(t *testing.T) {
	svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		verifyAdmin(t, call)
		want := `--command=CREATE DATABASE "sales""; DROP DATABASE reporting; --" OWNER "Analytics Team";`
		if !slices.Contains(call.Args, want) {
			t.Fatalf("expected quoted SQL %q, got %q", want, call.Args)
		}
		return nil
	})}
	if err := svc.CreateDatabase(context.Background(), testCluster, `sales"; DROP DATABASE reporting; --`, "Analytics Team"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAccountUsesNativePasswordAndTransaction(t *testing.T) {
	var calls int
	var output, stderr bytes.Buffer
	stop := errors.New("psql password prompt canceled")
	svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		calls++
		verifyAdmin(t, call)
		if !call.RequiresTTY || call.Stdout != &output || call.Stderr != &stderr {
			t.Fatalf("password command must use TTY and caller output: %#v", call)
		}
		for _, arg := range []string{
			"--single-transaction",
			`--command=CREATE ROLE "New"" Role" LOGIN;`,
			`--command=\password "New"" Role"`,
			`--command=GRANT CONNECT ON DATABASE "sales" TO "New"" Role";`,
		} {
			if !slices.Contains(call.Args, arg) {
				t.Errorf("missing %q in %q", arg, call.Args)
			}
		}
		if !strings.Contains(strings.Join(call.Args, " "), "rolpassword IS NOT NULL") {
			t.Errorf("missing an empty-password guard: %q", call.Args)
		}
		return stop
	})}
	if err := svc.CreateAccount(context.Background(), testCluster, "sales", `New" Role`, &output, &stderr); !errors.Is(err, stop) {
		t.Fatalf("error = %v, want %v", err, stop)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 psql transaction", calls)
	}
}

func TestRoleOperationsRejectUnsafeInputs(t *testing.T) {
	ctx := context.Background()
	svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		t.Fatalf("unexpected call: %#v", call)
		return nil
	})}
	if err := svc.ChangePassword(ctx, testCluster, "some\nrole", io.Discard, io.Discard); err == nil {
		t.Error("line breaks in a psql meta-command argument should fail")
	}
	if err := svc.SetConnect(ctx, testCluster, "sales", "public", true); err == nil {
		t.Error("PUBLIC should be ineligible for a change intended for one role")
	}
	if err := svc.CreateDatabase(ctx, testCluster, strings.Repeat("a", 64), ""); err == nil {
		t.Error("reject identifiers PostgreSQL would silently truncate")
	}
}

func TestLoginProbesSelectedClusterBeforeAskingForPassword(t *testing.T) {
	for _, tc := range []struct {
		name, inspect, probe string
		wantCalls            int
		wantError            string
	}{
		{"selected", "5442,localhost,17/reporting,123\n", "17/reporting,123,sales,report reader\n", 2, ""},
		{"socket only", "5442,,17/reporting,123\n", "", 1, "127.0.0.1"},
		{"port changed", "5432,localhost,17/reporting,123\n", "", 1, "端口"},
		{"wrong cluster", "5442,localhost,17/reporting,123\n", "17/other,124,sales,report reader\n", 2, "不一致"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			var report bytes.Buffer
			svc := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
				calls++
				if calls == 1 {
					verifyAdmin(t, call)
					_, err := fmt.Fprint(call.Stdout, tc.inspect)
					return err
				}
				if call.AsUser != "" || call.AsRoot || !call.RequiresTTY {
					t.Fatalf("app probe must be the caller and require a terminal: %#v", call)
				}
				for _, want := range []string{"host=127.0.0.1", "port=5442", "user='report reader'", "dbname='sales'"} {
					if !strings.Contains(strings.Join(call.Args, " "), want) {
						t.Errorf("probe args missing %q: %q", want, call.Args)
					}
				}
				if slices.Contains(call.Args, "--cluster") {
					t.Fatalf("unexpected app probe route: %q", call.Args)
				}
				_, err := fmt.Fprint(call.Stdout, tc.probe)
				return err
			})}
			err := svc.TestLogin(context.Background(), testCluster, "sales", "report reader", &report, io.Discard)
			if tc.wantError == "" {
				if err != nil || !strings.Contains(report.String(), "登录成功") {
					t.Fatalf("probe got %v, output %q", err, report.String())
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("probe error = %v, want %q", err, tc.wantError)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func verifyAdmin(t *testing.T, call command.Call) {
	t.Helper()
	if call.Name != "/usr/bin/psql" || call.AsUser != testCluster.Owner || call.AsRoot ||
		!strings.Contains(strings.Join(call.Args, " "), testCluster.ID()) ||
		!slices.Contains(call.Args, "-X") || !slices.Contains(call.Args, "--set=ON_ERROR_STOP=1") {
		t.Fatalf("unexpected admin command: %#v", call)
	}
}
