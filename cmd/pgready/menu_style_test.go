package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/png"
	"regexp"
	"strings"
	"testing"

	"github.com/9star-academy/PGReady/internal/command"
	"github.com/9star-academy/PGReady/internal/host"
)

func TestMenuStyleSelection(t *testing.T) {
	cases := []struct {
		name       string
		terminal   bool
		term       string
		colorterm  string
		noColor    bool
		wantHeader string
	}{
		{"redirected output", false, "xterm-256color", "truecolor", false, ""},
		{"NO_COLOR", true, "xterm-256color", "truecolor", true, ""},
		{"dumb terminal", true, "dumb", "truecolor", false, ""},
		{"unset TERM", true, "", "", false, ""},
		{"true color", true, "xterm-256color", "truecolor", false, "1;97;48;2;51;103;145"},
		{"direct color", true, "xterm-direct", "", false, "1;97;48;2;51;103;145"},
		{"Ghostty over SSH", true, "xterm-ghostty", "", false, "1;97;48;2;51;103;145"},
		{"256 colors", true, "xterm-256color", "", false, "1;97;48;5;24"},
		{"basic colors", true, "xterm", "", false, "1;97;44"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			style := chooseMenuStyle(tc.terminal, tc.term, tc.colorterm, tc.noColor)
			if style.header != tc.wantHeader {
				t.Fatalf("header = %q, want %q", style.header, tc.wantHeader)
			}
		})
	}
}

func TestStyledStatusPreservesMeaning(t *testing.T) {
	style := chooseMenuStyle(true, "xterm-256color", "truecolor", false)
	for _, tc := range []struct {
		status string
		color  string
	}{
		{"online", style.online},
		{"online,recovery", style.warning},
		{"down", style.offline},
		{"down,binaries_missing", style.offline},
		{"unknown", style.warning},
	} {
		want := "\x1b[" + tc.color + "m[" + tc.status + "]\x1b[0m"
		if got := style.status(tc.status); got != want {
			t.Errorf("status %q = %q, want %q", tc.status, got, want)
		}
	}
}

func TestMenuLogoCapability(t *testing.T) {
	for _, term := range []string{"xterm-kitty", "xterm-ghostty"} {
		if !chooseMenuStyle(true, term, "", false).logo {
			t.Errorf("logo disabled for %s", term)
		}
		if chooseMenuStyle(true, term, "", true).logo || chooseMenuStyle(false, term, "", false).logo {
			t.Errorf("logo escaped the plain output mode for %s", term)
		}
	}
	if chooseMenuStyle(true, "xterm-256color", "truecolor", false).logo {
		t.Fatal("sent graphics to a terminal with unknown image support")
	}
}

func TestMenuLogoSendsOfficialPNGAndReusesIt(t *testing.T) {
	var out bytes.Buffer
	a := app{out: &out}
	a.drawMenuLogo()
	chunks := regexp.MustCompile(`\x1b_G([^;]+);([A-Za-z0-9+/=]*)\x1b\\`).FindAllStringSubmatch(out.String(), -1)
	if len(chunks) < 2 || a.logoID == 0 {
		t.Fatal("expected chunked PNG transmission with an image ID")
	}
	wantCommand := fmt.Sprintf("a=T,f=100,i=%d,c=5,r=2,C=1,q=2,m=1", a.logoID)
	if chunks[0][1] != wantCommand {
		t.Fatalf("unexpected placement metadata %q", chunks[0][1])
	}
	var base64PNG strings.Builder
	for i, chunk := range chunks {
		wantFlag := "m=1"
		if i == len(chunks)-1 {
			wantFlag = "m=0"
		}
		if !strings.HasSuffix(chunk[1], wantFlag) || len(chunk[2]) > 4096 {
			t.Fatalf("invalid continuation chunk %d", i)
		}
		base64PNG.WriteString(chunk[2])
	}
	data, err := base64.StdEncoding.DecodeString(base64PNG.String())
	if err != nil || !bytes.Equal(data, postgresqlLogo) {
		t.Fatal("transmitted image differed from the embedded original")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 540 || config.Height != 557 {
		t.Fatalf("unexpected official image dimensions: %v, %v", config, err)
	}
	out.Reset()
	a.drawMenuLogo()
	want := fmt.Sprintf("\x1b_Ga=p,i=%d,c=5,r=2,C=1,q=2;\x1b\\", a.logoID)
	if out.String() != want {
		t.Fatalf("redraw = %q, want reusable placement %q", out.String(), want)
	}
}

func TestColorDoesNotChangeMenuActions(t *testing.T) {
	var out, errOut bytes.Buffer
	runner := command.Func(func(_ context.Context, call command.Call) error {
		if call.Name != "/usr/bin/pg_lsclusters" {
			t.Fatalf("unexpected command %s", call.Name)
		}
		_, err := fmt.Fprint(call.Stdout, inventory)
		return err
	})
	a := app{
		ctx: context.Background(), in: bufio.NewReader(strings.NewReader("1\n1\n2\n0\n8\n1\n0\n0\n")),
		out: &out, errOut: &errOut, host: host.Service{Runner: runner},
		style: chooseMenuStyle(true, "xterm-256color", "truecolor", false),
	}
	if err := a.mainMenu(); err != nil || errOut.Len() > 0 {
		t.Fatalf("main menu: %v, stderr %s", err, &errOut)
	}
	ansi := regexp.MustCompile(`\x1b\[[0-9;]+m`)
	got := ansi.ReplaceAllString(out.String(), "")
	for _, want := range []string{"作者：tangbaichuan", "GitHub：https://github.com/reedtang666", "1. 实例与服务",
		"8. 帮助与命令速查", "0. 退出", "[online]", "-p 5433 -U app_user",
		"输入数字后按 Enter，0 返回或退出。\n请选择："} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in menu output", want)
		}
	}
	if strings.Contains(got, "🐘") || strings.Contains(got, "\x1b_G") {
		t.Fatal("text terminal output should contain neither the emoji nor graphics escapes")
	}
}
