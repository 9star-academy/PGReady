package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const mainMenuTitle = "PGReady · PostgreSQL 管理器"

type menuStyle struct {
	header  string
	accent  string
	muted   string
	online  string
	offline string
	warning string
	logo    bool
}

func menuStyleFor(out io.Writer) menuStyle {
	file, ok := out.(*os.File)
	if !ok {
		return menuStyle{}
	}
	info, err := file.Stat()
	if err != nil {
		return menuStyle{}
	}
	return chooseMenuStyle(info.Mode()&os.ModeCharDevice != 0, os.Getenv("TERM"),
		os.Getenv("COLORTERM"), os.Getenv("NO_COLOR") != "")
}

func chooseMenuStyle(terminal bool, term, colorterm string, noColor bool) menuStyle {
	if !terminal || term == "" || term == "dumb" || noColor {
		return menuStyle{}
	}
	graphics := term == "xterm-kitty" || term == "xterm-ghostty"
	switch {
	case graphics, colorterm == "truecolor", colorterm == "24bit", strings.Contains(term, "direct"):
		// The title background is PostgreSQL's base blue (#336791).
		return menuStyle{
			header: "1;97;48;2;51;103;145", accent: "38;2;130;184;224", muted: "38;2;148;164;182",
			online: "1;38;2;136;202;157", offline: "1;38;2;235;145;132", warning: "38;2;233;187;116",
			logo: graphics,
		}
	case strings.Contains(term, "256color"):
		return menuStyle{
			header: "1;97;48;5;24", accent: "38;5;110", muted: "38;5;245",
			online: "1;38;5;114", offline: "1;38;5;210", warning: "38;5;222",
		}
	default:
		return menuStyle{
			header: "1;97;44", accent: "1;36", muted: "90",
			online: "1;32", offline: "1;31", warning: "33",
		}
	}
}

func paintMenu(text, color string) string {
	if color == "" {
		return text
	}
	return "\x1b[" + color + "m" + text + "\x1b[0m"
}

func (s menuStyle) status(status string) string {
	if s.header == "" {
		return status
	}
	color := s.warning
	switch {
	case status == "online":
		color = s.online
	case status == "down", strings.HasPrefix(status, "down,"):
		color = s.offline
	}
	return paintMenu("["+status+"]", color)
}

func (s menuStyle) title(title string) string {
	if s.header == "" {
		return title
	}
	return paintMenu("  "+title+"  ", s.header)
}

func (a *app) menuIntro(title string) {
	fmt.Fprintln(a.out)
	isMain := title == mainMenuTitle
	indent := ""
	if isMain {
		switch {
		case a.style.logo:
			a.drawMenuLogo()
			indent = strings.Repeat(" ", menuLogoColumns+1)
		case a.style.header != "":
			title = "🐘 " + title
		}
	}
	fmt.Fprintln(a.out, indent+a.style.title(title))
	if c := a.selected; c != nil {
		details := fmt.Sprintf("当前实例  %s  端口 %d", c.ID(), c.Port)
		fmt.Fprintf(a.out, "%s%s  %s\n", indent, paintMenu(details, a.style.muted), a.style.status(c.Status))
	} else if indent != "" {
		// Reserve the second row for the image even before an instance is selected.
		fmt.Fprintln(a.out)
	}
	if isMain {
		fmt.Fprintln(a.out, paintMenu("作者：", a.style.muted)+paintMenu("tangbaichuan", a.style.accent)+
			paintMenu("  |  GitHub：", a.style.muted)+paintMenu("https://github.com/reedtang666", a.style.accent))
	}
}

func (a *app) menuLine() {
	if a.style.accent != "" {
		fmt.Fprintln(a.out, paintMenu(strings.Repeat("─", 42), a.style.accent))
	}
}

func (a *app) menuItem(number int, label string) {
	n := fmt.Sprintf("%d.", number)
	if number == 0 {
		fmt.Fprintln(a.out, paintMenu(n+" "+label, a.style.muted))
		return
	}
	fmt.Fprintf(a.out, "%s %s\n", paintMenu(n, a.style.accent), label)
}

func (a *app) menuChoice() (string, error) {
	fmt.Fprintln(a.out, paintMenu("输入数字后按 Enter，0 返回或退出。", a.style.muted))
	return a.line(paintMenu("请选择：", a.style.accent))
}
