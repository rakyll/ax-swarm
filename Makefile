ADDR ?= 127.0.0.1:8788
AX ?= cli

.PHONY: dev

dev:
	node bin/ax-swarm.js serve --addr $(ADDR) --ax $(AX)
