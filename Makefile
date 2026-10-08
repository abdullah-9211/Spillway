# Loads .env if present (copy .env.example). Variables set in the shell win.
-include .env
export

GO ?= go
STATICCHECK ?= staticcheck
SQLC ?= sqlc

.PHONY: demo-data demo-data-clear up down migrate seed serve test test-unit test-integration lint sqlc web-install web-dev web-check web-build

up: ## start Postgres and Redis and wait until healthy
	docker compose up -d --wait

down:
	docker compose down

migrate: ## apply migrations to $$DATABASE_URL
	$(GO) run ./cmd/spillway migrate

seed: ## create the admin and viewer accounts from SEED_* in .env
	$(GO) run ./cmd/spillway seed

serve: ## run the Go service on :8080
	$(GO) run ./cmd/spillway serve

demo-data: ## sample usage on a key named demo-data, for trying the dashboard (invented numbers)
	docker compose exec -T postgres psql -U $${POSTGRES_USER:-spillway} -d $${POSTGRES_DB:-spillway} -v ON_ERROR_STOP=1 < deploy/demo-usage.sql

demo-data-clear:
	docker compose exec -T postgres psql -U $${POSTGRES_USER:-spillway} -d $${POSTGRES_DB:-spillway} -v ON_ERROR_STOP=1 < deploy/demo-usage-clear.sql

test: test-unit test-integration

test-unit:
	$(GO) test -race ./...

# Integration tests write real rows, so they get their own database and never touch the one the dashboard uses.
TEST_DB ?= spillway_test
TEST_DATABASE_URL ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/$(TEST_DB)?sslmode=disable

test-integration: ## needs `make up`
	@docker compose exec -T postgres psql -U $(POSTGRES_USER) -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$(TEST_DB)'" | grep -q 1 \
		|| docker compose exec -T postgres createdb -U $(POSTGRES_USER) $(TEST_DB)
	DATABASE_URL="$(TEST_DATABASE_URL)" $(GO) test -race -tags=integration -count=1 ./...

lint:
	$(GO) vet ./... && $(GO) vet -tags=integration ./...
	$(STATICCHECK) ./... && $(STATICCHECK) -tags=integration ./...

sqlc:
	$(SQLC) generate

web-install:
	cd web && npm install

web-dev: ## the dashboard on :3000, talking to the Go service
	cd web && npm run dev

web-build:
	cd web && npm run build

web-check: ## token drift, generated types, type-check, lint, unit tests
	cd web && npm run check
