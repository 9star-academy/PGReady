package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/9star-academy/PGReady/internal/command"
)

func TestInstallPackagesPreviewsOfficialStableVersions(t *testing.T) {
	for _, tc := range []struct {
		name, mode, first, selected string
		offline, failFirst          bool
	}{
		{name: "official", mode: "auto", first: pgdgRepository, selected: pgdgRepository},
		{name: "mirror after timeout", mode: "auto", first: pgdgRepository, selected: pgdgChinaRepository, failFirst: true},
		{name: "mirror first in cn", mode: "cn", first: pgdgChinaRepository, selected: pgdgChinaRepository},
		{name: "offline", mode: "auto", first: pgdgRepository, offline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PGREADY_DOWNLOAD_MODE", tc.mode)
			var previewDirs []string
			s := Service{files: testFS(), Runner: command.Func(func(_ context.Context, c command.Call) error {
				if c.AsRoot {
					t.Fatalf("version preview requested root: %+v", c)
				}
				switch c.Name {
				case aptCache:
					if c.Args[0] == "pkgnames" {
						return output(c, "postgresql-18\npostgresql-20\n")
					}
					return output(c, "postgresql-18:\n  Installed: (none)\n  Candidate: 18.6-1\n"+
						"postgresql-20:\n  Installed: (none)\n  Candidate: 20~beta1-1\n")
				case "/usr/bin/dpkg":
					return output(c, "arm64\n")
				case "/usr/bin/env":
					previewDir := filepath.Dir(strings.TrimPrefix(c.Args[0], "APT_CONFIG="))
					if c.Args[1] == aptGet {
						previewDirs = append(previewDirs, previewDir)
						if _, err := os.Stat(filepath.Join(previewDir, "lists", "partial")); err != nil {
							t.Fatalf("APT lists directory missing: %v", err)
						}
						source, err := os.ReadFile(filepath.Join(previewDir, "pgdg.sources"))
						if err != nil || !strings.Contains(string(source), "Suites: noble-pgdg\nArchitectures: arm64\nComponents: main\nSigned-By:\n -----BEGIN PGP PUBLIC KEY BLOCK-----\n .\n") {
							t.Fatalf("invalid preview source: %s, %v", source, err)
						}
						repository := tc.first
						if len(previewDirs) > 1 {
							repository = pgdgChinaRepository
						}
						if !strings.Contains(string(source), "URIs: https://"+repository+"\n") {
							t.Fatalf("preview did not use %s: %s", repository, source)
						}
						if tc.offline || (tc.failFirst && len(previewDirs) == 1) {
							return context.DeadlineExceeded
						}
						return nil
					}
					if c.Args[2] == "pkgnames" {
						return output(c, "postgresql-16\npostgresql-17\npostgresql-18\npostgresql-19\n")
					}
					return output(c, "postgresql-16:\n  Candidate: 16.16-1.pgdg24.04+1\n"+
						"postgresql-17:\n  Candidate: 17.12-1.pgdg24.04+1\n"+
						"postgresql-18:\n  Candidate: 18.7-1.pgdg24.04+1\n"+
						"postgresql-19:\n  Candidate: 19.0-1.pgdg24.04+1\n")
				default:
					t.Fatalf("unexpected preview call: %+v", c)
					return nil
				}
			})}
			got, err := s.InstallPackages(context.Background())
			want := []Package{
				{Major: "19", Candidate: "19.0-1.pgdg24.04+1", PGDG: true},
				{Major: "18", Candidate: "18.7-1.pgdg24.04+1", PGDG: true},
				{Major: "17", Candidate: "17.12-1.pgdg24.04+1", PGDG: true},
				{Major: "16", Candidate: "16.16-1.pgdg24.04+1", PGDG: true},
			}
			for i := range want {
				want[i].pgdgRepository = tc.selected
			}
			if tc.offline {
				want = []Package{{Major: "18", Candidate: "18.6-1"}}
			}
			if !reflect.DeepEqual(got, want) || (err != nil) != tc.offline {
				t.Fatalf("InstallPackages() = %+v, %v; want %+v, offline=%v", got, err, want, tc.offline)
			}
			for _, dir := range previewDirs {
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("preview directory should be cleaned up: %v", err)
				}
			}
		})
	}
}

func TestInstallPGDGUsesConfirmedVersion(t *testing.T) {
	const version = "17.12-1.pgdg24.04+1"
	for _, tc := range []struct {
		name        string
		existingPG  bool
		existingAPT bool
		repository  string
		missing     bool
		wantSteps   []string
		wantError   string
	}{
		{name: "fresh host", wantSteps: []string{"source", "update", "install"}},
		{name: "selected mirror", repository: pgdgChinaRepository, wantSteps: []string{"source", "update", "install"}},
		{name: "already has PGDG", existingAPT: true, wantSteps: []string{"update", "install"}},
		{name: "selected package disappeared", missing: true, wantSteps: []string{"source", "update"}, wantError: "请重新选择"},
		{name: "already has PostgreSQL", existingPG: true, wantError: "本机已有 PostgreSQL 实例"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository := tc.repository
			if repository == "" {
				repository = pgdgRepository
			}
			var steps []string
			installed := false
			cluster := testCluster
			cluster.Version = "17"
			s := Service{files: testFS(), Runner: command.Func(func(_ context.Context, c command.Call) error {
				switch c.Name {
				case listTool:
					if tc.existingPG || installed {
						return output(c, inventory(cluster))
					}
					return fs.ErrNotExist
				case dpkg:
					return output(c, "")
				case "/usr/bin/dpkg":
					return output(c, "amd64\n")
				case "/bin/sh":
					steps = append(steps, "source")
					source, err := io.ReadAll(c.Stdin)
					if err != nil || !c.AsRoot || !strings.Contains(string(source), fmt.Sprintf("URIs: https://%s\nSuites: noble-pgdg\n", repository)) {
						t.Fatalf("unexpected source setup: %+v, %v", c, err)
					}
					return nil
				case aptCache:
					if c.Args[0] == "show" {
						if c.Args[1] != "postgresql-17="+version {
							t.Fatalf("wrong confirmed package %q", c.Args[1])
						}
						if tc.missing {
							return errors.New("version not found")
						}
						return output(c, "Package: postgresql-17\nVersion: "+version+"\n")
					}
					return output(c, "postgresql-17:\n  Installed: (none)\n  Candidate: 17.13-1\n")
				case aptGet:
					switch c.Args[0] {
					case "indextargets":
						if tc.existingAPT {
							return output(c, "http://apt.postgresql.org/pub/repos/apt/ noble-pgdg\n")
						}
						return output(c, "http://archive.ubuntu.com/ubuntu/ noble\n")
					case "update":
						if !c.AsRoot {
							t.Fatal("system update did not request root")
						}
						steps = append(steps, "update")
						return nil
					default:
						if !c.AsRoot || !reflect.DeepEqual(c.Args, []string{"--yes", "--no-remove", "--no-upgrade", "install", "postgresql-17=" + version}) {
							t.Fatalf("unexpected install call: %+v", c)
						}
						steps = append(steps, "install")
						installed = true
						return nil
					}
				default:
					t.Fatalf("unexpected command: %+v", c)
					return nil
				}
			})}
			err := s.Install(context.Background(), Package{Major: "17", Candidate: version, PGDG: true, pgdgRepository: tc.repository}, io.Discard, io.Discard)
			if !reflect.DeepEqual(steps, tc.wantSteps) || (tc.wantError == "" && err != nil) || (tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError))) {
				t.Fatalf("install steps = %v, error = %v; want %v, %q", steps, err, tc.wantSteps, tc.wantError)
			}
		})
	}
}
