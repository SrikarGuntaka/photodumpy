# photo-organizer -- developer entry points.
#
# Every target here works with only Docker installed; a local Go toolchain is
# optional and only makes the `go-*` targets faster.

SHELL := /bin/sh
COMPOSE := docker compose
GO_IMAGE := golang:1.25-alpine
# Run a throwaway Go container over the working tree, reusing the module cache
# in a named volume so repeated invocations are fast.
GO_RUN := docker run --rm -v "$(CURDIR)":/src -v photo-organizer-gomod:/go/pkg/mod -w /src $(GO_IMAGE)

.DEFAULT_GOAL := help

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'

## up: build and start the full stack in the background
up:
	$(COMPOSE) up -d --build

## up-fg: start the stack in the foreground (Ctrl-C to stop)
up-fg:
	$(COMPOSE) up --build

## down: stop the stack, keeping data
down:
	$(COMPOSE) down

## reset: stop the stack and DELETE the database volume
reset:
	$(COMPOSE) down -v

## logs: tail logs from all services
logs:
	$(COMPOSE) logs -f

## ps: show service status
ps:
	$(COMPOSE) ps

## status: run the CLI status command inside the api container
status:
	$(COMPOSE) exec api /usr/local/bin/photo-organizer status

## psql: open a psql shell against the running database
psql:
	$(COMPOSE) exec postgres psql -U $${POSTGRES_USER:-photo} -d $${POSTGRES_DB:-photoorganizer}

## tidy: resolve dependencies and write go.sum (uses Docker; no local Go needed)
tidy:
	$(GO_RUN) go mod tidy

## build: compile all binaries into ./bin (uses Docker)
build:
	$(GO_RUN) sh -c 'CGO_ENABLED=0 go build -o bin/api ./cmd/api && CGO_ENABLED=0 go build -o bin/worker ./cmd/worker && CGO_ENABLED=0 go build -o bin/photo-organizer ./cmd/cli'

## vet: run go vet (uses Docker)
vet:
	$(GO_RUN) go vet ./...

## fmt: format all Go source (uses Docker)
fmt:
	$(GO_RUN) gofmt -w -l .

## test: run unit tests (uses Docker)
test:
	$(GO_RUN) go test ./...

## fixtures: regenerate the synthetic test corpus in ./sample-photos
fixtures:
	$(GO_RUN) go run ./cmd/genfixtures -root ./sample-photos -clean

## test-integration: run tests that need a real Postgres, in an isolated database
##
## Uses a SEPARATE database from the running app. Sharing one is a trap: the
## live worker containers poll the same queue, claim the tests' synthetic jobs,
## find no matching photos and kill them. Tests then fail with "work was lost"
## for reasons that have nothing to do with the code under test.
##
## -p 1 serialises packages, since they share the one test database.
test-integration:
	$(COMPOSE) up -d postgres
	$(COMPOSE) exec -T postgres psql -U $${POSTGRES_USER:-photo} -d postgres -c "SELECT 1 FROM pg_database WHERE datname='photoorganizer_test'" | grep -q 1 || 		$(COMPOSE) exec -T postgres psql -U $${POSTGRES_USER:-photo} -d postgres -c "CREATE DATABASE photoorganizer_test OWNER $${POSTGRES_USER:-photo}"
	docker run --rm --network photo-organizer_default -v "$(CURDIR)":/src -v photo-organizer-gomod:/go/pkg/mod -w /src 		-e TEST_DATABASE_URL='postgres://$${POSTGRES_USER:-photo}:$${POSTGRES_PASSWORD:-photo}@postgres:5432/photoorganizer_test?sslmode=disable' 		$(GO_IMAGE) go test -tags integration -count=1 -p 1 -timeout 15m ./...

## test-integration-reset: drop the test database so the next run starts clean
test-integration-reset:
	$(COMPOSE) exec -T postgres psql -U $${POSTGRES_USER:-photo} -d postgres -c "DROP DATABASE IF EXISTS photoorganizer_test"

## scan: register and scan the sample corpus through the running API
scan:
	$(COMPOSE) exec api /usr/local/bin/photo-organizer scan /photos -wait

.PHONY: help up up-fg down reset logs ps status psql tidy build vet fmt test fixtures test-integration test-integration-reset scan
