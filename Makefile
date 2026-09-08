SHELL := /bin/bash
PATH := $(shell go env GOPATH)/bin:$(PATH)

.PHONY: all build test test-race lint clean

all: build

build:
	go build -o bin/runnel ./cmd/runnel
	go build -o bin/runnel-get ./cmd/runnel-get

test:
	go test -v ./...

test-race:
	go test -race -v ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/ dist/
