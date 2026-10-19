.PHONY: build test vet fmt check linux test-pg

build:
	go build -o bin/sumrak-api ./cmd/syncd

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

check: fmt vet test

# Cross-compile for the home server (see deploy/README.md; GOARCH=arm64 for ARM boxes).
linux:
	GOOS=linux GOARCH=amd64 go build -o bin/sumrak-api-linux-amd64 ./cmd/syncd

# Integration tests against a throwaway database on local Postgres 17
# (brew services start postgresql@17). Creates + drops its own DB.
PGBIN ?= /opt/homebrew/opt/postgresql@17/bin
test-pg:
	@$(PGBIN)/pg_isready -q || { echo "Postgres not running: brew services start postgresql@17"; exit 1; }
	@db=sumrak_syncd_test_$$$$; \
	$(PGBIN)/createdb $$db && \
	{ SYNCD_TEST_DATABASE_URL="postgres://$$(whoami)@localhost:5432/$$db?sslmode=disable" go test -count=1 ./...; rc=$$?; \
	$(PGBIN)/dropdb --if-exists $$db; exit $$rc; }
