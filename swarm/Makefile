.PHONY: build install test

build:
	go build -o ax-swarm .

install:
	go build -o $(shell go env GOPATH)/bin/ax-swarm .

test:
	go test -race -v ./...
