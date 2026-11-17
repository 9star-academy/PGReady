package pg

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/9star-academy/PGReady/internal/cluster"
	"github.com/9star-academy/PGReady/internal/command"
)

func (s Service) Config(ctx context.Context, c cluster.Cluster, view string) (string, error) {
	switch view {
	case "paths":
		return s.query(ctx, c, maintenanceDB, `
SELECT name, setting AS path FROM pg_catalog.pg_settings
WHERE name IN ('data_directory', 'config_file', 'hba_file') ORDER BY name;`)
	case "listen":
		return s.query(ctx, c, maintenanceDB, `
SELECT name, setting AS value FROM pg_catalog.pg_settings
WHERE name IN ('listen_addresses', 'port', 'unix_socket_directories') ORDER BY name;`)
	case "hba":
		rules, err := s.query(ctx, c, maintenanceDB, `
SELECT rule_number, file_name, line_number, type, database, user_name,
       address, netmask, auth_method, error
FROM pg_catalog.pg_hba_file_rules
ORDER BY rule_number NULLS LAST, file_name, line_number;`)
		if err != nil {
			return "", err
		}
		return "以下为磁盘上的认证规则及解析结果。请结合最近重载时间确认生效状态。\n" + rules, nil
	case "settings":
		return s.query(ctx, c, maintenanceDB, `
SELECT name, setting, unit, source, sourcefile, sourceline, pending_restart
FROM pg_catalog.pg_settings
WHERE name IN ('max_connections', 'shared_buffers', 'work_mem', 'maintenance_work_mem',
               'effective_cache_size', 'wal_level', 'max_wal_size', 'logging_collector',
               'log_destination', 'log_min_duration_statement', 'log_min_duration_sample')
ORDER BY name;`)
	default:
		return "", fmt.Errorf("未知的配置查询 %q", view)
	}
}

func (s Service) Diagnostics(ctx context.Context, c cluster.Cluster, view string) (string, error) {
	switch view {
	case "logs":
		logs, err := s.recentLog(ctx, c)
		if err != nil {
			return "", err
		}
		return lastLines(logs, 80), nil
	case "slow":
		return s.slowQueries(ctx, c)
	case "connections":
		return s.query(ctx, c, maintenanceDB, `
SELECT datname AS database, count(*) AS connections,
       count(*) FILTER (WHERE state = 'active') AS active,
       max(pg_catalog.clock_timestamp() - xact_start) AS oldest_transaction
FROM pg_catalog.pg_stat_activity
WHERE backend_type = 'client backend' AND pid <> pg_catalog.pg_backend_pid()
GROUP BY datname ORDER BY datname;`)
	case "locks":
		return s.query(ctx, c, maintenanceDB, `
SELECT a.pid AS waiting_pid, b.pid AS blocking_pid, a.datname AS database,
       a.usename AS waiting_user, a.wait_event,
       pg_catalog.clock_timestamp() - a.query_start AS query_age
FROM pg_catalog.pg_stat_activity a
CROSS JOIN LATERAL unnest(pg_catalog.pg_blocking_pids(a.pid)) AS b(pid)
WHERE a.wait_event_type = 'Lock' AND a.pid <> pg_catalog.pg_backend_pid()
ORDER BY a.query_start;`)
	case "disk":
		if !filepath.IsAbs(c.DataDir) {
			return "", fmt.Errorf("无法确认集群 %s 的数据目录", c.ID())
		}
		databases, err := s.ListDatabases(ctx, c)
		if err != nil {
			return "", err
		}
		disk, err := command.Output(ctx, s.Runner, command.Call{
			Name: "/usr/bin/df", Args: []string{"-hP", "--", c.DataDir}, AsUser: c.Owner,
		})
		if err != nil {
			return "", err
		}
		return databases + "\n" + disk, nil
	default:
		return "", fmt.Errorf("未知的诊断操作 %q", view)
	}
}

func (s Service) Extensions(ctx context.Context, c cluster.Cluster, database string, enabled bool) (string, error) {
	if enabled {
		if _, err := cluster.QuoteIdentifier(database); err != nil {
			return "", err
		}
		return s.query(ctx, c, database, `
SELECT e.extname AS extension, e.extversion AS version, n.nspname AS schema
FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
ORDER BY e.extname;`)
	}
	return s.query(ctx, c, maintenanceDB, `
SELECT name, default_version, comment
FROM pg_catalog.pg_available_extensions ORDER BY name;`)
}

// A configured file path outside the standard APT location uses the invoking
// account's file permissions. An elevated read only targets the default log.
func (s Service) recentLog(ctx context.Context, c cluster.Cluster) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	defaultLog := filepath.Join("/var/log/postgresql", "postgresql-"+c.Version+"-"+c.Name+".log")
	path := c.LogFile
	if path != defaultLog && (c.Status == "online" || strings.HasPrefix(c.Status, "online,")) {
		call, err := psql(c, maintenanceDB, []string{"--csv", "--tuples-only"}, `
SELECT COALESCE(pg_catalog.pg_current_logfile(), ''), pg_catalog.current_setting('data_directory');`)
		if err != nil {
			return "", err
		}
		fields, err := s.readCSVRow(ctx, call, 2)
		if err != nil {
			return "", err
		}
		if fields[0] != "" {
			path = fields[0]
			if !filepath.IsAbs(path) {
				path = filepath.Join(fields[1], path)
			}
		}
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("无法定位集群 %s 的日志文件，请检查日志收集器或系统日志", c.ID())
	}
	var owner string
	stat := os.Stat
	if path == defaultLog {
		owner = c.Owner
		stat = os.Lstat // Elevated reads must not follow a symlink at the default path.
	}
	info, err := stat(path)
	if err != nil {
		return "", fmt.Errorf("读取集群日志 %s: %w", c.ID(), err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("集群 %s 的日志路径不是普通文件", c.ID())
	}
	return command.Output(ctx, s.Runner, command.Call{
		Name: "/usr/bin/tail", Args: []string{"--bytes=65536", "--", path}, AsUser: owner,
	})
}

var durationInLog = regexp.MustCompile(`(?:LOG:\s+|,"|"message"\s*:\s*")duration:\s+([0-9]+(?:\.[0-9]+)?)\s+ms\b`)

func (s Service) slowQueries(ctx context.Context, c cluster.Cluster) (string, error) {
	call, err := psql(c, maintenanceDB, []string{"--csv", "--tuples-only"}, `
WITH thresholds AS (
  SELECT max(setting::integer) FILTER (WHERE name = 'log_min_duration_statement') AS statement_ms,
         max(setting::integer) FILTER (WHERE name = 'log_min_duration_sample') AS sample_ms,
         max(setting::numeric) FILTER (WHERE name = 'log_statement_sample_rate') AS sample_rate
  FROM pg_catalog.pg_settings
  WHERE name IN ('log_min_duration_statement', 'log_min_duration_sample', 'log_statement_sample_rate')
)
SELECT statement_ms, sample_ms, sample_rate,
       COALESCE(LEAST(NULLIF(statement_ms, -1), CASE WHEN sample_rate > 0 THEN NULLIF(sample_ms, -1) END), -1)
FROM thresholds;`)
	if err != nil {
		return "", err
	}
	values, err := s.readCSVRow(ctx, call, 4)
	if err != nil {
		return "", err
	}
	minMS, err := strconv.Atoi(values[3])
	if err != nil {
		return "", fmt.Errorf("无效的慢查询阈值: %w", err)
	}
	if minMS < 0 {
		return "当前实例未启用基于耗时阈值的慢查询日志。\n", nil
	}

	logs, err := s.recentLog(ctx, c)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, line := range strings.Split(logs, "\n") {
		match := durationInLog.FindStringSubmatch(line)
		if len(match) < 2 {
			continue
		}
		ms, err := strconv.ParseFloat(match[1], 64)
		if err == nil && ms >= float64(minMS) {
			matches = append(matches, line)
		}
	}
	summary := fmt.Sprintf("慢查询阈值 %s ms，抽样阈值 %s ms，抽样率 %s。\n", values[0], values[1], values[2])
	if len(matches) == 0 {
		return summary + "最近日志中未找到达到阈值的耗时记录。\n", nil
	}
	return summary + lastLines(strings.Join(matches, "\n"), 80), nil
}

func lastLines(logs string, limit int) string {
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return strings.Join(lines, "\n") + "\n"
}
