BINARY := photo_copy_check
GO     := go
ARGS   ?=

.PHONY: all build run install clean test fmt vet

all: build

# Phony build: Go's build cache makes this a no-op when nothing changed, so we
# don't need to enumerate source-file prerequisites by hand.
build:
	$(GO) build -o "$(BINARY)" .

run: build
	"./$(BINARY)" $(ARGS)

install:
	$(GO) install .

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -f "$(BINARY)"
