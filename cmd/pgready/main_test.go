package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/9star-academy/PGReady/internal/command"
	"github.com/9star-academy/PGReady/internal/host"
)

const inventory = `Ver Cluster Port Status Owner    Data directory              Log file
16  main    5432 online postgres /var/lib/postgresql/16/main /var/log/postgresql/postgresql-16-main.log
16  test    5433 online postgres /var/lib/postgresql/16/test /var/log/postgresql/postgresql-16-test.log
`

func TestVersionCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--version"}, strings.NewReader(""), &out, &errOut, nil)
	if want := "pgready " + version + "\n"; code != 0 || out.String() != want || errOut.Len() != 0 {
		t.Fatalf("version command returned %d, stdout %q, stderr %q", code, &out, &errOut)
	}
}

func TestMenuHelpUsesChosenPort(t *testing.T) {
	var out, errOut bytes.Buffer
	runner := command.Func(func(_ context.Context, call command.Call) error {
		if call.Name != "/usr/bin/pg_lsclusters" || call.AsRoot || call.AsUser != "" {
			t.Fatalf("unexpected command: %+v", call)
		}
		_, err := fmt.Fprint(call.Stdout, inventory)
		return err
	})
	input := strings.NewReader("1\n1\n2\n0\n8\n1\n2\n0\n0\n")
	code := run(context.Background(), nil, input, &out, &errOut, runner)
	for _, want := range []string{"8. 帮助与命令速查", "已选择 16/test", "-p 5433 -U app_user", `\conninfo`, "0. 退出",
		"输入数字后按 Enter，0 返回或退出。\n请选择："} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q from menu output:\n%s", want, &out)
		}
	}
	if strings.Contains(out.String(), "🐘") {
		t.Fatal("redirected output should not contain the fallback logo")
	}
	if code != 0 || errOut.Len() != 0 {
		t.Fatalf("run returned %d, stderr %q", code, &errOut)
	}
}

func TestStopIsScopedToConfirmedInstance(t *testing.T) {
	var out, errOut bytes.Buffer
	var stops [][]string
	runner := command.Func(func(_ context.Context, call command.Call) error {
		switch call.Name {
		case "/usr/bin/pg_lsclusters":
			_, err := fmt.Fprint(call.Stdout, inventory)
			return err
		case "/usr/bin/pg_ctlcluster":
			if !call.AsRoot {
				t.Fatal("instance control did not request system privileges")
			}
			stops = append(stops, call.Args)
			return nil
		default:
			t.Fatalf("unexpected command: %+v", call)
			return nil
		}
	})
	input := strings.NewReader("1\n1\n2\n4\nn\n4\ny\n0\n0\n")
	code := run(context.Background(), nil, input, &out, &errOut, runner)
	if code != 0 || errOut.Len() > 0 || !reflect.DeepEqual(stops, [][]string{{"16", "test", "stop"}}) {
		t.Fatalf("run returned %d, stops %v, stderr %q", code, stops, &errOut)
	}
	if !strings.Contains(out.String(), "已取消") {
		t.Fatal("the first stop should have been canceled")
	}
}

func TestChooseInstallPackage(t *testing.T) {
	packages := []host.Package{
		{Major: "17", Candidate: "17.9-1"},
		{Major: "18", Candidate: "18.6-0ubuntu0.26.04.1"},
	}
	for _, tc := range []struct {
		name   string
		input  string
		prompt string
		cancel bool
	}{
		{name: "confirm second version", input: "2\ny\n", prompt: "确认通过 APT 安装 PostgreSQL 18（18.6-0ubuntu0.26.04.1）？ [y/N]："},
		{name: "invalid choice then confirm", input: "3\n2\ny\n", prompt: "确认通过 APT 安装 PostgreSQL 18（18.6-0ubuntu0.26.04.1）？ [y/N]："},
		{name: "Enter cancels confirmation", input: "2\n\n", prompt: "确认通过 APT 安装 PostgreSQL 18（18.6-0ubuntu0.26.04.1）？ [y/N]：", cancel: true},
		{name: "zero returns before confirmation", input: "0\n", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			a := app{ctx: context.Background(), in: bufio.NewReader(strings.NewReader(tc.input)), out: &out, errOut: &errOut}
			selected, err := a.chooseInstallPackage(packages)
			if tc.cancel {
				if !errors.Is(err, errCanceled) || selected != (host.Package{}) {
					t.Fatalf("canceled choice returned %+v, %v", selected, err)
				}
			} else if err != nil || selected != packages[1] {
				t.Fatalf("confirmed choice returned %+v, %v; want %+v", selected, err, packages[1])
			}
			for _, item := range []string{"1. PostgreSQL 17（17.9-1）", "2. PostgreSQL 18（18.6-0ubuntu0.26.04.1）", "0. 返回"} {
				if !strings.Contains(out.String(), item) {
					t.Errorf("missing %q from output:\n%s", item, &out)
				}
			}
			if tc.prompt == "" {
				if strings.Contains(out.String(), "[y/N]") {
					t.Errorf("return should skip confirmation:\n%s", &out)
				}
			} else if !strings.Contains(out.String(), tc.prompt) {
				t.Errorf("missing confirmation %q from output:\n%s", tc.prompt, &out)
			}
		})
	}
}

func TestDatabaseWriteStopsOnMismatchedDataDirectory(t *testing.T) {
	var out, errOut bytes.Buffer
	writeAttempted := false
	runner := command.Func(func(_ context.Context, call command.Call) error {
		if call.Name == "/usr/bin/pg_lsclusters" {
			_, err := fmt.Fprint(call.Stdout, inventory)
			return err
		}
		if call.Name == "/usr/bin/psql" && reflect.DeepEqual(call.Args[len(call.Args)-2:], []string{"-c", "SHOW data_directory"}) {
			_, err := fmt.Fprintln(call.Stdout, "/var/lib/postgresql/16/main")
			return err
		}
		writeAttempted = true
		return nil
	})
	input := strings.NewReader("1\n1\n2\n0\n3\n2\nnewdb\n\ny\n0\n0\n")
	code := run(context.Background(), nil, input, &out, &errOut, runner)
	if code != 0 || writeAttempted || !strings.Contains(errOut.String(), "数据目录与集群 16/test 的记录不符") {
		t.Fatalf("run returned %d, write attempted %v, stderr %q", code, writeAttempted, &errOut)
	}
}

func TestEndOfInputReturnsWithoutAction(t *testing.T) {
	var out, errOut bytes.Buffer
	runner := command.Func(func(_ context.Context, call command.Call) error {
		if call.Name != "/usr/bin/pg_lsclusters" {
			t.Fatal(call.Name)
		}
		_, err := fmt.Fprint(call.Stdout, inventory)
		return err
	})
	if code := run(context.Background(), nil, strings.NewReader("8\n"), &out, &errOut, runner); code != 0 || errOut.Len() != 0 {
		t.Fatalf("run returned %d, stderr %q", code, &errOut)
	}
}
