package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestOutputPreservesCommandError(t *testing.T) {
	failure := errors.New("exit status 1")
	runner := Func(func(_ context.Context, call Call) error {
		fmt.Fprintln(call.Stdout, "partial result")
		fmt.Fprintln(call.Stderr, "connection failed")
		return failure
	})
	got, err := Output(context.Background(), runner, Call{Name: "psql"})
	if got != "partial result\n" || !errors.Is(err, failure) || !strings.Contains(err.Error(), "connection failed") {
		t.Fatalf("Output = %q, %v", got, err)
	}
}

func TestExecDoesNotInheritPostgresTarget(t *testing.T) {
	t.Setenv("PGHOST", "other-server")
	t.Setenv("PGPORT", "9999")
	t.Setenv("PGSERVICE", "production")
	out, err := Output(context.Background(), Exec{}, Call{Name: "/usr/bin/env"})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "PG") {
			t.Fatalf("unexpected PostgreSQL environment variable %q", strings.SplitN(line, "=", 2)[0])
		}
	}
}
