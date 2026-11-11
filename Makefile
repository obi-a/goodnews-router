GO ?= go
# Keep package discovery in source directories, separate from lab artifacts.
PACKAGES = . ./cmd/... ./drivers/... ./internal/...

.PHONY: build test vet check fuzz lab-up lab-check lab-down integration

build:
	$(GO) build -o bin/goodnews ./cmd/goodnews
	$(GO) build -o bin/badnews ./cmd/badnews

test:
	$(GO) test -race $(PACKAGES)

vet:
	$(GO) vet $(PACKAGES)

check: test vet

fuzz:
	$(GO) test -run='^$$' -fuzz=FuzzParseIPv4 -fuzztime=10s -parallel=2 .
	$(GO) test -run='^$$' -fuzz=FuzzEthernetARP -fuzztime=10s -parallel=2 .

lab-up:
	sudo bash lab/stage1.sh up

lab-check:
	sudo bash lab/stage1.sh check

lab-down:
	sudo bash lab/stage1.sh down

integration: build
	sudo python3 lab/acceptance.py

.PHONY: lab2-up lab2-check lab2-down integration2
lab2-up:
	sudo bash lab/stage2.sh up

lab2-check:
	sudo bash lab/stage2.sh check

lab2-down:
	sudo bash lab/stage2.sh down

integration2: build
	sudo python3 lab/acceptance_stage2.py
