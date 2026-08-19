// File overview: One server per database, and what that must not break.

package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"rolltop/backend/store/pgtestdb"
)

// TestExclusiveInstanceRefusesASecondServer is the guard the SQLite file locks
// used to provide for free. Without it both processes start, each start marks
// the other's in-flight sync runs interrupted, and both fetch every mailbox.
func TestExclusiveInstanceRefusesASecondServer(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	first, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err == nil {
		second.Close()
		t.Fatal("a second server opened the same database")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("the refusal does not say what is wrong: %v", err)
	}
}

// TestExclusiveInstanceIsReleasedOnClose is what makes a restart work at all:
// the lock has to be gone the moment the previous store closes, not whenever
// its connection is eventually recycled.
func TestExclusiveInstanceIsReleasedOnClose(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	first, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err != nil {
		t.Fatalf("the lock outlived the store that held it: %v", err)
	}
	defer second.Close()
}

// TestExclusiveInstanceWaitsForTheOutgoingServer covers the rolling deploy: the
// old container is still serving while the new one starts, so refusing at once
// would turn every deployment into a crash loop.
func TestExclusiveInstanceWaitsForTheOutgoingServer(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	outgoing, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		closed <- outgoing.Close()
	}()

	incoming, err := OpenPostgres(ctx, dsn, PostgresOptions{
		MaxConns: 2, DataDir: t.TempDir(), ExclusiveInstance: true, InstanceLockWait: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("the incoming server did not wait for the outgoing one: %v", err)
	}
	defer incoming.Close()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// TestStoresWithoutExclusiveInstanceShareADatabase pins that the guard is
// opt-in. The test suite opens several stores against one database on purpose,
// and so does every test that reopens a store to check persistence.
func TestStoresWithoutExclusiveInstanceShareADatabase(t *testing.T) {
	ctx := context.Background()
	dsn := pgtestdb.NewFromTemplate(t, SchemaTag(), buildTestTemplate)

	first, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenPostgres(ctx, dsn, PostgresOptions{MaxConns: 2, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
}
