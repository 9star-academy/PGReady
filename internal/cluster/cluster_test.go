package cluster

import (
	"reflect"
	"testing"
)

func TestClientTargetsClusterAndQuotesDatabase(t *testing.T) {
	c := Cluster{Version: "16", Name: "other", Port: 5433, Owner: "postgres"}
	got, err := c.Client("pg_dump", "test db='x'\\p", "--format=custom")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--cluster", "16/other", "--username=postgres", "--no-password", "--dbname=dbname='test db=\\'x\\'\\\\p'", "--format=custom"}
	if got.Name != "/usr/bin/pg_dump" || got.AsUser != "postgres" || !reflect.DeepEqual(got.Args, want) {
		t.Fatalf("unexpected call: %#v", got)
	}
}

func TestClientRejectsRemoteClusterSyntax(t *testing.T) {
	c := Cluster{Version: "16", Name: "remote:5432", Port: 5433, Owner: "postgres"}
	if _, err := c.Client("psql", "postgres"); err == nil {
		t.Fatal("accepted host:port as a local cluster")
	}
}

func TestQuoteIdentifier(t *testing.T) {
	q, err := QuoteIdentifier(`test"db`)
	if err != nil || q != `"test""db"` {
		t.Fatalf("QuoteIdentifier = %q, %v", q, err)
	}
	if _, err := QuoteIdentifier("bad\x00name"); err == nil {
		t.Fatal("accepted a NUL in a PostgreSQL name")
	}
}
