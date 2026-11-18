GO ?= go
TESTFLAGS ?= -race -count=1
FUZZ_TIME ?= 10s
LEFT_SUBNET ?= 10.0.0.0/24
RIGHT_SUBNET ?= 10.0.1.0/24
MODE ?= physical
export LEFT_DEVICE RIGHT_DEVICE LEFT_SUBNET RIGHT_SUBNET MODE
# Keep package discovery in source directories, separate from lab artifacts.
PACKAGES = . ./cmd/... ./drivers/... ./internal/...

.PHONY: build test vet lint python-test check fuzz lab-up lab-check lab-down integration

build:
	$(GO) build -o bin/goodnews ./cmd/goodnews
	$(GO) build -o bin/badnews ./cmd/badnews

test:
	$(GO) test $(TESTFLAGS) $(PACKAGES)

vet:
	$(GO) vet $(PACKAGES)

lint:
	python3 lab/check_sources.py
	$(GO) mod verify

python-test:
	python3 -B -m unittest discover -s lab -p 'test_*.py' -v

check: test vet lint python-test

fuzz:
	$(GO) test -run='^$$' -fuzz=FuzzParseIPv4 -fuzztime=$(FUZZ_TIME) -parallel=2 .
	$(GO) test -run='^$$' -fuzz=FuzzEthernetARP -fuzztime=$(FUZZ_TIME) -parallel=2 .
	$(GO) test -run='^$$' -fuzz=FuzzDecodeRouting -fuzztime=$(FUZZ_TIME) -parallel=2 .

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

.PHONY: lab3-preflight lab3-up lab3-check lab3-down integration3 workflow-lint
lab3-preflight:
	sudo python3 -B lab/stage3.py preflight --mode "$$MODE" --left-device "$$LEFT_DEVICE" --right-device "$$RIGHT_DEVICE" --left-subnet "$$LEFT_SUBNET" --right-subnet "$$RIGHT_SUBNET"

lab3-up:
	sudo python3 -B lab/stage3.py up --mode "$$MODE" --left-device "$$LEFT_DEVICE" --right-device "$$RIGHT_DEVICE" --left-subnet "$$LEFT_SUBNET" --right-subnet "$$RIGHT_SUBNET"

lab3-check:
	sudo python3 -B lab/stage3.py check

lab3-down:
	sudo python3 -B lab/stage3.py down

# Synthetic lifecycle regression, separate from the physical Stage 3 exit gate.
integration3: build
	sudo python3 -B lab/acceptance_stage3.py

workflow-lint:
	actionlint

ALGORITHM ?= dv
.PHONY: lab4-up lab4-start lab4-stop lab4-check lab4-down integration4
lab4-up: build
	sudo python3 -B lab/stage4.py up --algorithm $(ALGORITHM)

lab4-start:
	sudo python3 -B lab/stage4.py start

lab4-stop:
	sudo python3 -B lab/stage4.py stop

lab4-check:
	sudo python3 -B lab/stage4.py check

lab4-down:
	sudo python3 -B lab/stage4.py down

integration4: build
	sudo python3 -B lab/acceptance_stage4.py
