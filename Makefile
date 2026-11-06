PREFIX ?= $(HOME)/.local

install:
	go build -o copilot-usage .
	install -d $(PREFIX)/bin
	install -m 755 copilot-usage $(PREFIX)/bin/copilot-usage

.PHONY: install
