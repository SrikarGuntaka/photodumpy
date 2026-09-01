# photo-organizer -- developer entry points.
#
# Every target here works with only Docker installed; a local Go toolchain is
# optional and only makes the `go-*` targets faster.

SHELL := /bin/sh
COMPOSE := docker compose
GO_IMAGE := golang:1.23-alpine
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

.PHONY: help up up-fg down reset logs ps status psql tidy build vet fmt test
