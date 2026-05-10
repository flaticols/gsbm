# gsbm developer entrypoints. Tests/lint stay accessible as `go test ./...`
# directly — these targets exist for the workflows that have non-trivial
# flag combinations (benchmarks, fuzz harnesses).

GO          ?= go
BENCHTIME   ?= 3x
FUZZTIME    ?= 30s
FUZZ_TIMEOUT ?= 300s

# (package, fuzz function) pairs. Go fuzz only runs one function at a
# time, so we iterate. Keep this list in sync with the harnesses listed
# in docs/plans/.../*-bench-and-fuzz-large-payload.md.
FUZZ_TARGETS := \
	storage/gsbm:FuzzReaderRobustness \
	storage/gsbm:FuzzWriterReaderRoundTripCanonical \
	storage/gsbm:FuzzHeaderCorruption \
	tools/gsbmcodegen/fixtures/sample:FuzzArenaDecodeAgainstHeap

.PHONY: test bench fuzz vet help

help:
	@echo "Targets:"
	@echo "  test     - go test ./... (unit + budget guards)"
	@echo "  vet      - go vet ./..."
	@echo "  bench    - run all Benchmark* with -benchmem (BENCHTIME=$(BENCHTIME))"
	@echo "  fuzz     - run each Fuzz* harness for FUZZTIME=$(FUZZTIME)"

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

bench:
	$(GO) test -bench='^Benchmark' -benchmem -benchtime=$(BENCHTIME) -run='^$$' ./...

fuzz:
	@set -e; for target in $(FUZZ_TARGETS); do \
		pkg=$${target%%:*}; \
		fn=$${target##*:}; \
		echo "==> $$pkg :: $$fn (fuzztime=$(FUZZTIME))"; \
		$(GO) test -run='^$$' -fuzz="^$$fn$$" -fuzztime=$(FUZZTIME) -timeout=$(FUZZ_TIMEOUT) ./$$pkg; \
	done
