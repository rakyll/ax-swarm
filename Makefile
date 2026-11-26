.PHONY: build install test

build:
	go build -o axo .

install:
	go build -o $(shell go env GOPATH)/bin/axo .

test:
	go test -race -v ./...
