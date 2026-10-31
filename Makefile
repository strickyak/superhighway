.PHONY: all help run-server test

GOWORK ?= off
PASSWORD ?= PW123

all: help

help:
	@echo "Superhighway Makefile targets:"
	@echo "  make help        Show this help message (default)"
	@echo "  make run-server  Run superhighway-server with --fs=./fs/ --password=$(PASSWORD) on localhost:1908"
	@echo "  make test        Run all Go unit tests"

run-server:
	mkdir -p ./fs/
	GOWORK=$(GOWORK) go run ./cmd/superhighway-server --fs=./fs/ --addr=localhost:1908 --password=$(PASSWORD)

test:
	GOWORK=$(GOWORK) go test -v ./...
