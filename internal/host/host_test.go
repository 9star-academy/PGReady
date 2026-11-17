package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
)

const (
	aptCache = "/usr/bin/apt-cache"
	aptGet   = "/usr/bin/apt-get"
	dpkg     = "/usr/bin/dpkg-query"
	listTool = "/usr/bin/pg_lsclusters"
)

var testCluster = cluster.Cluster{
	Version: "16",
	Name:    "main",
	Port:    5433,
	Status:  "down",
	Owner:   "postgres",
	DataDir: "/var/lib/Postgres Data/16/main",
	LogFile: "/var/log/postgresql/16 main.log",
}

func inventory(rows ...cluster.Cluster) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%-3s %-12s %-5s %-18s %-10s %-40s %s\n",
		"Ver", "Cluster", "Port", "Status", "Owner", "Data directory", "Log file")
	for _, c := range rows {
		fmt.Fprintf(&out, "%-3s %-12s %-5d %-18s %-10s %-40s %s\n",
			c.Version, c.Name, c.Port, c.Status, c.Owner, c.DataDir, c.LogFile)
	}
	return out.String()
}

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"etc/os-release": {Data: []byte("ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04 LTS\"\n")},
		"proc/1/comm":    {Data: []byte("systemd\n")},
	}
}

func output(call command.Call, value string) error {
	_, err := io.WriteString(call.Stdout, value)
	return err
}

func TestClustersPreservesInstancePathsAndOrder(t *testing.T) {
	other := cluster.Cluster{Version: "17", Name: "app", Port: 5432, Status: "online,recovery", Owner: "postgres",
		DataDir: "/var/lib/postgresql/17/app", LogFile: "/var/log/postgresql/17 app.log"}
	service := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
		if call.Name != listTool || len(call.Args) != 0 || call.AsRoot {
			t.Fatalf("unexpected inventory call: %+v", call)
		}
		return output(call, inventory(testCluster, other))
	})}

	got, err := service.Clusters(context.Background())
	if err != nil || !reflect.DeepEqual(got, []cluster.Cluster{testCluster, other}) {
		t.Fatalf("Clusters() = %+v, %v", got, err)
	}
}

func TestClustersRejectsBrokenInventory(t *testing.T) {
	for _, got := range []string{
		"",
		"unexpected header\n",
		inventory() + "16 main 5433 down postgres /path /log\n",
		inventory(testCluster, testCluster),
	} {
		s := Service{Runner: command.Func(func(_ context.Context, c command.Call) error {
			return output(c, got)
		})}
		if _, err := s.Clusters(context.Background()); err == nil {
			t.Errorf("accepted broken inventory %q", got)
		}
	}
}

func TestClustersDistinguishesEmptyAndUnrecognized(t *testing.T) {
	for _, tc := range []struct {
		name     string
		listErr  error
		process  string
		dpkgInfo string
		binary   bool
		wantErr  string
	}{
		{name: "fresh", listErr: fs.ErrNotExist},
		{name: "empty existing tools"},
		{name: "discovery permission", listErr: fs.ErrPermission, wantErr: "pg_lsclusters"},
		{name: "unregistered process", listErr: fs.ErrNotExist, process: "postgres\n", wantErr: "进程"},
		{name: "unregistered package", dpkgInfo: "postgresql-17\tii \n", wantErr: "postgresql-17"},
		{name: "unregistered binary", listErr: fs.ErrNotExist, binary: true, wantErr: "/usr/local/pgsql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := testFS()
			if tc.process != "" {
				files["proc/42/comm"] = &fstest.MapFile{Data: []byte(tc.process)}
			}
			if tc.binary {
				files["usr/local/pgsql/bin/postgres"] = &fstest.MapFile{}
			}
			s := Service{files: files, Runner: command.Func(func(_ context.Context, c command.Call) error {
				switch c.Name {
				case listTool:
					if tc.listErr != nil {
						return tc.listErr
					}
					return output(c, inventory())
				case dpkg:
					return output(c, "postgresql-client-16\tii \n"+tc.dpkgInfo)
				default:
					t.Fatalf("unexpected command: %+v", c)
					return nil
				}
			})}
			got, err := s.Clusters(context.Background())
			if len(got) != 0 || (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr))) {
				t.Fatalf("Clusters() = %+v, %v; want error containing %q", got, err, tc.wantErr)
			}
		})
	}
}

func TestControlRechecksSelectedInstanceAndScopesCommand(t *testing.T) {
	for _, tc := range []struct {
		name       string
		action     string
		targetPort int
		wantAction bool
	}{
		{"restart expected instance", "restart", 5433, true},
		{"port changed", "stop", 5434, false},
		{"unsupported action", "promote", 5433, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := testCluster
			current.Port = tc.targetPort
			var actionCalled bool
			s := Service{Runner: command.Func(func(_ context.Context, call command.Call) error {
				if call.Name == listTool {
					return output(call, inventory(current))
				}
				actionCalled = true
				if call.Name != "/usr/bin/pg_ctlcluster" || !reflect.DeepEqual(call.Args, []string{"16", "main", tc.action}) || !call.AsRoot {
					t.Fatalf("unexpected control call: %+v", call)
				}
				return nil
			})}
			err := s.Control(context.Background(), testCluster, tc.action, io.Discard, io.Discard)
			if actionCalled != tc.wantAction || (err == nil) != tc.wantAction {
				t.Fatalf("actionCalled = %v; err = %v", actionCalled, err)
			}
		})
	}
}

func TestReadyChecksClusterSocketAsOwner(t *testing.T) {
	refused := errors.New("exit status 1")
	s := Service{Runner: command.Func(func(_ context.Context, c command.Call) error {
		wantArgs := []string{"--cluster", testCluster.ID(), "--username=postgres", "--dbname=postgres", "--timeout=3"}
		if c.Name != "/usr/bin/pg_isready" || !reflect.DeepEqual(c.Args, wantArgs) || c.AsUser != testCluster.Owner || c.AsRoot {
			t.Fatalf("unexpected readiness probe: %+v", c)
		}
		if err := output(c, "/its own/socket:5433 - rejecting connections\n"); err != nil {
			return err
		}
		return refused
	})}
	got, err := s.Ready(context.Background(), testCluster)
	if !errors.Is(err, refused) || !strings.Contains(got, "rejecting connections") {
		t.Fatalf("Ready() = %q, %v", got, err)
	}
}

func TestEnvironmentUsesHostVersionAndConfiguredOrigins(t *testing.T) {
	s := Service{files: testFS(), Runner: command.Func(func(_ context.Context, call command.Call) error {
		if call.Name != aptCache || !reflect.DeepEqual(call.Args, []string{"policy"}) {
			t.Fatalf("unexpected APT call: %+v", call)
		}
		return output(call, "Package files:\n    500 https://archive.ubuntu.com/ubuntu noble/main arm64 Packages\n     origin archive.ubuntu.com\n")
	})}
	info, err := s.Environment(context.Background())
	if err != nil || !strings.Contains(info, "Ubuntu 24.04 LTS") || !strings.Contains(info, "archive.ubuntu.com") {
		t.Fatalf("Environment() = %q, %v", info, err)
	}
}

func TestPackagesFiltersVersionedServerCandidates(t *testing.T) {
	s := Service{files: testFS(), Runner: command.Func(func(_ context.Context, c command.Call) error {
		if c.Name != aptCache || c.AsRoot {
			t.Fatalf("unexpected APT call: %+v", c)
		}
		switch c.Args[0] {
		case "pkgnames":
			return output(c, "postgresql-17\npostgresql-16\npostgresql-17-dbgsym\npostgresql\npostgresql-9\npostgresql-client-17\n")
		case "policy":
			if !reflect.DeepEqual(c.Args, []string{"policy", "postgresql-9", "postgresql-16", "postgresql-17"}) {
				t.Fatalf("unexpected apt policy: %+v", c.Args)
			}
			return output(c, "postgresql-9:\n  Installed: (none)\n  Candidate: (none)\n"+
				"postgresql-16:\n  Installed: 16.1-1\n  Candidate: 16.2-1\n"+
				"postgresql-17:\n  Installed: (none)\n  Candidate: 17.2-1\n")
		default:
			t.Fatalf("unexpected APT args: %v", c.Args)
			return nil
		}
	})}
	want := []Package{{Major: "16", Installed: "16.1-1", Candidate: "16.2-1"}, {Major: "17", Candidate: "17.2-1"}}
	got, err := s.Packages(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Packages() = %+v, %v; want %+v", got, err, want)
	}
}

func TestPackageMissingAPTMetadataIncludesRecovery(t *testing.T) {
	s := Service{files: testFS(), Runner: command.Func(func(_ context.Context, c command.Call) error {
		return output(c, "")
	})}
	_, err := s.Package(context.Background(), "19")
	if err == nil || !strings.Contains(err.Error(), "apt-get update") {
		t.Fatalf("missing metadata error = %v", err)
	}
}

func TestInstallGuardsAndPartialSuccess(t *testing.T) {
	installErr := errors.New("APT lock unavailable")
	for _, tc := range []struct {
		name        string
		unsafeOS    bool
		existing    bool
		noCandidate bool
		noCluster   bool
		packageErr  error
		wantInstall bool
		wantErr     string
	}{
		{name: "success", wantInstall: true},
		{name: "unsupported", unsafeOS: true, wantErr: "Ubuntu 24.04"},
		{name: "existing registration", existing: true, wantErr: "已有"},
		{name: "not available", noCandidate: true, wantErr: "apt-get update"},
		{name: "apt failure", wantInstall: true, packageErr: installErr, wantErr: installErr.Error()},
		{name: "package but no cluster", wantInstall: true, noCluster: true, wantErr: "软件包"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := testFS()
			if tc.unsafeOS {
				files["etc/os-release"] = &fstest.MapFile{Data: []byte("ID=debian\nVERSION_ID=12\n")}
			}
			installed := false
			s := Service{files: files, Runner: command.Func(func(_ context.Context, c command.Call) error {
				switch c.Name {
				case listTool:
					if tc.existing || (installed && !tc.noCluster) {
						return output(c, inventory(testCluster))
					}
					return fs.ErrNotExist
				case dpkg:
					if installed {
						return output(c, "postgresql-16\tii \n")
					}
					return output(c, "")
				case aptCache:
					candidate := "16.7-1"
					if tc.noCandidate {
						candidate = "(none)"
					}
					return output(c, "postgresql-16:\n  Installed: (none)\n  Candidate: "+candidate+"\n")
				case aptGet:
					if installed || !c.AsRoot || !reflect.DeepEqual(c.Args, []string{"--yes", "--no-remove", "--no-upgrade", "install", "postgresql-16=16.7-1"}) {
						t.Fatalf("unexpected install command: %+v", c)
					}
					installed = true
					return tc.packageErr
				default:
					t.Fatalf("unexpected call: %+v", c)
					return nil
				}
			})}
			err := s.Install(context.Background(), "16", io.Discard, io.Discard)
			if installed != tc.wantInstall || (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr))) {
				t.Fatalf("Install() = %v, attempted %v; want error containing %q and attempted %v", err, installed, tc.wantErr, tc.wantInstall)
			}
		})
	}
}
