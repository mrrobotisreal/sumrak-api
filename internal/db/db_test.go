package db

import (
	"context"
	"os"
	"sync"
	"testing"
)

func TestMigrations_embeddedAndOrdered(t *testing.T) {
	migs, err := loadMigrations()
	if err != nil || len(migs) == 0 {
		t.Fatalf("migrations: %v %v", migs, err)
	}
	if migs[0].version != "0001_init" {
		t.Fatalf("first = %s", migs[0].version)
	}
}

func TestMigrate_idempotentAndConcurrent(t *testing.T) {
	url := os.Getenv("SYNCD_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SYNCD_TEST_DATABASE_URL unset; run `make test-pg`")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := Migrate(ctx, pool); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	again, err := Migrate(ctx, pool)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run applied %v err %v", again, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('users','devices','records','schema_migrations')`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("tables = %d err %v", n, err)
	}
}
