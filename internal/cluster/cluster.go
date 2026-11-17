package cluster

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/9star-academy/PGReady/internal/command"
)

type Cluster struct {
	Version string
	Name    string
	Status  string
	Owner   string
	DataDir string
	LogFile string
	Port    int
}

var (
	versionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	namePattern    = regexp.MustCompile(`^[[:alnum:]_][[:alnum:]_.-]*$`)
	ownerPattern   = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]*\$?$`)
	conninfoEscape = strings.NewReplacer(`\`, `\\`, `'`, `\'`)
)

func (c Cluster) ID() string {
	return c.Version + "/" + c.Name
}

func (c Cluster) Validate() error {
	if !versionPattern.MatchString(c.Version) || !namePattern.MatchString(c.Name) {
		return fmt.Errorf("无法识别本机 PostgreSQL 集群 %q", c.ID())
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("集群 %s 的端口无效", c.ID())
	}
	if !ownerPattern.MatchString(c.Owner) || c.Owner == "root" {
		return fmt.Errorf("集群 %s 的系统所有者不可用", c.ID())
	}
	return nil
}

// Client uses Debian's wrapper to resolve the selected local cluster.
func (c Cluster) Client(tool, database string, args ...string) (command.Call, error) {
	if err := c.Validate(); err != nil {
		return command.Call{}, err
	}
	switch tool {
	case "psql", "pg_dump", "pg_restore":
	default:
		return command.Call{}, fmt.Errorf("不支持的 PostgreSQL 工具 %q", tool)
	}
	clientArgs := []string{"--cluster", c.ID(), "--username=" + c.Owner, "--no-password"}
	if database != "" {
		if _, err := QuoteIdentifier(database); err != nil {
			return command.Call{}, err
		}
		// A raw database name containing '=' can be interpreted as libpq parameters.
		clientArgs = append(clientArgs, "--dbname=dbname='"+conninfoEscape.Replace(database)+"'")
	}
	return command.Call{
		Name:   "/usr/bin/" + tool,
		Args:   append(clientArgs, args...),
		AsUser: c.Owner,
	}, nil
}

func QuoteIdentifier(name string) (string, error) {
	if len(name) == 0 || len(name) > 63 || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("数据库对象名称需要使用 1～63 字节有效 UTF-8 字符")
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}
