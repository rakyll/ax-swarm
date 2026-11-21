.PHONY: build test install clean

BIN_DIR := bin

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/ax-swarm ./cmd/ax-swarm
	go build -o $(BIN_DIR)/ax-swarm-worker ./cmd/ax-swarm-worker

test:
	go test -race -v ./...

install:
	go install ./cmd/ax-swarm
	go install ./cmd/ax-swarm-worker

clean:
	rm -rf $(BIN_DIR)
