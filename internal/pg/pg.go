package pg

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
)

type Service struct {
	Runner command.Runner
}

const maintenanceDB = "postgres"

func psql(c cluster.Cluster, database string, options []string, statements ...string) (command.Call, error) {
	args := append([]string{"-X", "--set=ON_ERROR_STOP=1", "--pset=pager=off"}, options...)
	for _, sql := range statements {
		args = append(args, "--command="+sql)
	}
	return c.Client("psql", database, args...)
}

func (s Service) query(ctx context.Context, c cluster.Cluster, database, sql string) (string, error) {
	call, err := psql(c, database, nil, sql)
	if err != nil {
		return "", err
	}
	return command.Output(ctx, s.Runner, call)
}

const databaseListSQL = `
SELECT datname AS database, pg_catalog.pg_get_userbyid(datdba) AS owner,
       pg_catalog.pg_size_pretty(pg_catalog.pg_database_size(oid)) AS size
FROM pg_catalog.pg_database
WHERE NOT datistemplate
ORDER BY datname;`

func (s Service) ListDatabases(ctx context.Context, c cluster.Cluster) (string, error) {
	return s.query(ctx, c, maintenanceDB, databaseListSQL)
}

func (s Service) CreateDatabase(ctx context.Context, c cluster.Cluster, name, owner string) error {
	database, err := cluster.QuoteIdentifier(name)
	if err != nil {
		return err
	}
	sql := "CREATE DATABASE " + database
	if owner != "" {
		quotedOwner, err := roleIdentifier(owner)
		if err != nil {
			return err
		}
		sql += " OWNER " + quotedOwner
	}
	_, err = s.query(ctx, c, maintenanceDB, sql+";")
	return err
}

func (s Service) ListRoles(ctx context.Context, c cluster.Cluster) (string, error) {
	return s.query(ctx, c, maintenanceDB, `
SELECT r.rolname AS role, r.rolcanlogin AS login, r.rolsuper AS superuser,
       r.rolcreatedb AS create_db, r.rolcreaterole AS create_role,
       r.rolreplication AS replication, r.rolbypassrls AS bypass_rls,
       r.rolinherit AS inherit, r.rolvaliduntil AS valid_until,
       ARRAY(SELECT DISTINCT pg_catalog.pg_get_userbyid(m.roleid)
               FROM pg_catalog.pg_auth_members m WHERE m.member = r.oid
               ORDER BY 1) AS member_of
FROM pg_catalog.pg_roles r
ORDER BY r.rolname;`)
}

func roleIdentifier(name string) (string, error) {
	// PostgreSQL parses even the quoted name "public" as the pseudo-role PUBLIC.
	if name == "public" {
		return "", fmt.Errorf("PUBLIC 表示所有角色，请输入单个角色名")
	}
	return cluster.QuoteIdentifier(name)
}

func (s Service) ConnectPrivileges(ctx context.Context, c cluster.Cluster, database string) (string, error) {
	if _, err := cluster.QuoteIdentifier(database); err != nil {
		return "", err
	}
	return s.query(ctx, c, maintenanceDB, `
SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC'
            ELSE pg_catalog.pg_get_userbyid(a.grantee)::text END AS grantee,
       pg_catalog.pg_get_userbyid(a.grantor) AS grantor, a.is_grantable
FROM pg_catalog.pg_database d
CROSS JOIN LATERAL pg_catalog.aclexplode(COALESCE(d.datacl, pg_catalog.acldefault('d', d.datdba))) a
WHERE d.datname = `+sqlString(database)+` AND a.privilege_type = 'CONNECT'
ORDER BY 1, 2;`)
}

func (s Service) SetConnect(ctx context.Context, c cluster.Cluster, database, role string, grant bool) error {
	db, r, err := accessIdentifiers(database, role)
	if err != nil {
		return err
	}
	sql := "GRANT CONNECT ON DATABASE " + db + " TO " + r + ";"
	if !grant {
		sql = "REVOKE CONNECT ON DATABASE " + db + " FROM " + r + ";"
	}
	_, err = s.query(ctx, c, maintenanceDB, sql)
	return err
}

func accessIdentifiers(database, role string) (string, string, error) {
	db, err := cluster.QuoteIdentifier(database)
	if err != nil {
		return "", "", err
	}
	r, err := roleIdentifier(role)
	return db, r, err
}

func (s Service) CreateAccount(ctx context.Context, c cluster.Cluster, database, role string, out, errOut io.Writer) error {
	db, r, err := accessIdentifiers(database, role)
	if err != nil {
		return err
	}
	return s.passwordOperation(ctx, c, role, out, errOut, "CREATE ROLE "+r+" LOGIN;", "GRANT CONNECT ON DATABASE "+db+" TO "+r+";")
}

func (s Service) ChangePassword(ctx context.Context, c cluster.Cluster, role string, out, errOut io.Writer) error {
	return s.passwordOperation(ctx, c, role, out, errOut)
}

var sqlStringEscape = strings.NewReplacer("\\", "\\\\", "'", "''")

func sqlString(value string) string {
	return "E'" + sqlStringEscape.Replace(value) + "'"
}

func (s Service) passwordOperation(ctx context.Context, c cluster.Cluster, role string, out, errOut io.Writer, before ...string) error {
	quotedRole, err := roleIdentifier(role)
	if err != nil {
		return err
	}
	if strings.ContainsAny(role, "\r\n") {
		return fmt.Errorf("密码操作的角色名不能含换行符")
	}

	// psql prompts through the TTY, then sends an encrypted password. The check
	// keeps a canceled or empty password from committing the account change.
	assertion := "DO " + sqlString(`
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_catalog.pg_authid
     WHERE rolname = `+sqlString(role)+` AND rolpassword IS NOT NULL
  ) THEN
    RAISE EXCEPTION 'Password must not be empty';
  END IF;
END;`) + ";"
	statements := append(before, "\\password "+quotedRole, assertion)
	call, err := psql(c, maintenanceDB, []string{"--single-transaction"}, statements...)
	if err != nil {
		return err
	}
	call.RequiresTTY, call.Stdout, call.Stderr = true, out, errOut
	return s.Runner.Run(ctx, call)
}

const serverIdentitySQL = "pg_catalog.current_setting('cluster_name'), EXTRACT(EPOCH FROM pg_catalog.pg_postmaster_start_time())"

func (s Service) TestLogin(ctx context.Context, c cluster.Cluster, database, role string, out, errOut io.Writer) error {
	if _, _, err := accessIdentifiers(database, role); err != nil {
		return err
	}
	call, err := psql(c, maintenanceDB, []string{"--csv", "--tuples-only"},
		"SELECT pg_catalog.current_setting('port'), pg_catalog.current_setting('listen_addresses'), "+serverIdentitySQL+";")
	if err != nil {
		return err
	}
	expected, err := s.readCSVRow(ctx, call, 4)
	if err != nil {
		return fmt.Errorf("确认目标集群的监听地址: %w", err)
	}
	port := strconv.Itoa(c.Port)
	if expected[0] != port {
		return fmt.Errorf("目标集群的实际端口 %s 与选定端口 %s 不同，请重新选择实例", expected[0], port)
	}
	if !listensOnLoopback(expected[1]) {
		return fmt.Errorf("集群 %s 未配置监听 IPv4 回环地址 127.0.0.1", c.ID())
	}

	conninfo := "host=127.0.0.1 port=" + port + " connect_timeout=5 passfile=/dev/null" +
		" user=" + conninfoValue(role) + " dbname=" + conninfoValue(database)
	call = command.Call{
		Name: "/usr/bin/psql",
		// --host bypasses pg_wrapper's default cluster selection.
		Args: []string{"-X", "--set=ON_ERROR_STOP=1", "--host=127.0.0.1", "--password",
			"--csv", "--tuples-only", "--dbname=" + conninfo,
			"--command=SELECT " + serverIdentitySQL + ", pg_catalog.current_database(), SESSION_USER;"},
		RequiresTTY: true,
		Stderr:      errOut,
	}
	actual, err := s.readCSVRow(ctx, call, 4)
	if err != nil {
		return fmt.Errorf("应用账号连接或查询失败: %w", err)
	}
	if actual[0] != expected[2] || actual[1] != expected[3] || actual[2] != database || actual[3] != role {
		return fmt.Errorf("连接结果与选定集群、数据库或角色不一致")
	}
	_, err = fmt.Fprintf(out, "登录成功。集群 %s，数据库 %q，账号 %q。\n", c.ID(), database, role)
	return err
}

func (s Service) readCSVRow(ctx context.Context, call command.Call, columns int) ([]string, error) {
	data, err := command.Output(ctx, s.Runner, call)
	if err != nil {
		return nil, err
	}
	csvReader := csv.NewReader(strings.NewReader(data))
	csvReader.FieldsPerRecord = columns
	rows, err := csvReader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("预期一行 PostgreSQL 查询结果，实际 %d 行", len(rows))
	}
	return rows[0], nil
}

func listensOnLoopback(list string) bool {
	for _, address := range strings.Split(list, ",") {
		switch strings.TrimSpace(address) {
		case "*", "0.0.0.0", "localhost", "127.0.0.1":
			return true
		}
	}
	return false
}

var conninfoEscape = strings.NewReplacer("\\", "\\\\", "'", "\\'")

func conninfoValue(value string) string {
	return "'" + conninfoEscape.Replace(value) + "'"
}
