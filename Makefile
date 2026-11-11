GO ?= go
# Keep package discovery in source directories, separate from lab artifacts.
PACKAGES = . ./cmd/... ./drivers/... ./internal/...

.PHONY: build test vet check fuzz lab-up lab-check lab-down integration

build:
	$(GO) build -o bin/goodnews ./cmd/goodnews

test:
	$(GO) test -race $(PACKAGES)

vet:
	$(GO) vet $(PACKAGES)

check: test vet

fuzz:
	$(GO) test -run='^$$' -fuzz=FuzzParseIPv4 -fuzztime=10s -parallel=2 .

lab-up:
	sudo bash lab/stage1.sh up

lab-check:
	sudo bash lab/stage1.sh check

lab-down:
	sudo bash lab/stage1.sh down

integration: build
	sudo python3 lab/acceptance.py
