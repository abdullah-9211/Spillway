# Loads .env if present (copy .env.example). Variables set in the shell win.
-include .env
export

GO ?= go
STATICCHECK ?= staticcheck
SQLC ?= sqlc

.PHONY: up down migrate seed serve test test-unit test-integration lint sqlc web-install web-dev web-check web-build

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

test: test-unit test-integration

test-unit:
	$(GO) test -race ./...

test-integration: ## needs `make up`
	$(GO) test -race -tags=integration -count=1 ./...

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
