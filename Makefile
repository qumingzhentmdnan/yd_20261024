GO ?= go

.PHONY: test vet race

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

race:
	CGO_ENABLED=1 $(GO) test -race ./...
