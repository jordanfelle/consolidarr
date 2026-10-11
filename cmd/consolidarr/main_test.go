package main

import (
	"errors"
	"testing"
	"time"
)

func TestOpenWithRetryFailsFastOnNonTransientAndRetriesTransient(t *testing.T) {
	// SQLite or a non-postgres URL never retries: a bad path errors at once
	start := time.Now()
	if _, err := openWithRetry("/nonexistent/definitely/not/here", "", 30*time.Second); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("sqlite failure must be immediate, err=%v after %s", err, time.Since(start))
	}
	// a Postgres DSN pointing at a closed port is transient: it keeps trying until the wait is up
	start = time.Now()
	_, err := openWithRetry(t.TempDir(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1", 3*time.Second)
	if err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) < 2*time.Second {
		t.Fatalf("a refused connection must be retried until the deadline, gave up after %s: %v", time.Since(start), err)
	}
}

func TestTransient(t *testing.T) {
	if transient(errors.New("schema: syntax error at or near \"CREAT\"")) {
		t.Fatal("a schema/migration error is not transient")
	}
	if !transient(errors.New("dial tcp 10.0.0.1:5432: connect: connection refused")) || !transient(errors.New("FATAL: the database system is starting up")) {
		t.Fatal("connection refused / starting up are transient")
	}
}
