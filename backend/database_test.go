package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func freshTestDatabase(t *testing.T, environment string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(environment)
	if url == "" {
		t.Skip("Set " + environment + " to an empty isolated database")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var name string
	if err = pool.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil || !strings.HasSuffix(name, "_test") {
		t.Fatal("Test database name must end in _test")
	}
	var existing bool
	if err = pool.QueryRow(ctx, `SELECT to_regclass('administrators') IS NOT NULL`).Scan(&existing); err != nil || existing {
		t.Fatal("Test requires a fresh database")
	}
	if _, err = pool.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	return pool
}

// Exercise concurrent persistence without changing password cost or HTTP deadlines.
func TestRuntimeDatabase(t *testing.T) {
	s := &server{db: freshTestDatabase(t, "TEST_RUNTIME_DATABASE_URL")}
	testMaintenanceIndexesDatabase(t, s)
	testAlertRecoveryDatabase(t, s)
	testESIMCardCommitDatabase(t, s)
	testDeveloperModuleSnapshotDatabase(t, s)
	testCallRecordRecoveryDatabase(t, s)
	testSIPSnapshotDatabase(t, s)
	testSIPAdmissionDatabase(t, s)
	testSIPAccountMutationDatabase(t, s)
	testControlWritesDoNotHoldStateLock(t, s)
	testMessageScaleDatabase(t, s)
	testDeveloperMessageParity(t, s)
	testReceiptPollingDatabase(t, s)
	testMessageTimeoutDatabase(t, s)
	testMessageThreadsDatabase(t, s)
	testDeveloperRuntimeDatabase(t, s)
	testModuleNetworkDatabase(t, s)
}
