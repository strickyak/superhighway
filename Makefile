.PHONY: all help run-server test

GOWORK ?= off
PASSWORD ?= PW123
RO_PASSWORD ?= RO123

all: help

help:
	@echo "Superhighway Makefile targets:"
	@echo "  make help        Show this help message (default)"
	@echo "  make server      Compiile the server into build/superhighway-server"
	@echo "  make run-server  Run superhighway-server with --fs=./fs/ --password=$(PASSWORD) --ro-password=$(RO_PASSWORD) on localhost:1908"
	@echo "  make test        Run all Go unit tests"

run-server:
	mkdir -p ./fs/
	GOWORK=$(GOWORK) go run ./cmd/superhighway-server --fs=./fs/ --addr=localhost:1908 --password=$(PASSWORD) --ro-password=$(RO_PASSWORD)


server:
	mkdir -p ./fs/
	GOWORK=$(GOWORK) go build -o build/superhighway-server ./cmd/superhighway-server

test:
	GOWORK=$(GOWORK) go test -v ./...
