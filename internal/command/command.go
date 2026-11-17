package command

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Call describes a direct command invocation with optional system privileges.
type Call struct {
	Name        string
	Args        []string
	AsUser      string
	AsRoot      bool
	RequiresTTY bool
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

type Runner interface {
	Run(context.Context, Call) error
}

type Func func(context.Context, Call) error

func (f Func) Run(ctx context.Context, call Call) error {
	return f(ctx, call)
}

type Exec struct{}

func (Exec) Run(ctx context.Context, call Call) error {
	name, args := call.Name, call.Args
	if call.AsRoot && call.AsUser != "" {
		return fmt.Errorf("不能同时指定 root 和其他系统账号")
	}
	if call.AsRoot || call.AsUser != "" {
		if !filepath.IsAbs(name) {
			return fmt.Errorf("提权操作需要绝对命令路径")
		}
		targetUID := "0"
		if call.AsUser != "" {
			account, err := user.Lookup(call.AsUser)
			if err != nil {
				return fmt.Errorf("查找系统账号 %s: %w", call.AsUser, err)
			}
			targetUID = account.Uid
		}
		if targetUID != strconv.Itoa(os.Geteuid()) {
			args = append([]string{"-u", "#" + targetUID, "--", name}, args...)
			name = "/usr/bin/sudo"
		}
	}

	if call.RequiresTTY {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("此操作需要交互式终端: %w", err)
		}
		defer tty.Close()
		if call.Stdin == nil {
			call.Stdin = tty
		}
	}

	cmd := exec.CommandContext(ctx, name, args...)
	// sudo can relay SIGTERM to the client it started.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdin, cmd.Stdout, cmd.Stderr = call.Stdin, call.Stdout, call.Stderr
	for _, entry := range os.Environ() {
		// libpq must resolve the selected cluster using the explicit arguments.
		if !strings.HasPrefix(entry, "PG") && !strings.HasPrefix(entry, "LC_ALL=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C")
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w", filepath.Base(call.Name), err)
	}
	return nil
}

// Output returns stdout and, on error, includes stderr without hiding the cause.
func Output(ctx context.Context, runner Runner, call Call) (string, error) {
	var stdout, stderr bytes.Buffer
	call.Stdout = &stdout
	if call.Stderr == nil {
		call.Stderr = &stderr
	}
	err := runner.Run(ctx, call)
	if detail := strings.TrimSpace(stderr.String()); err != nil && detail != "" {
		err = fmt.Errorf("%w: %s", err, detail)
	}
	return stdout.String(), err
}
