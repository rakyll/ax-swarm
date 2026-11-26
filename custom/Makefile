.PHONY: build install test

build:
	go build -o ax-o .

install:
	go build -o $(shell go env GOPATH)/bin/ax-o .

test:
	go test -race -v ./...
