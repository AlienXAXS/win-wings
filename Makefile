# win-wings build targets.
#
# Two binaries are produced and must be deployed together in the same directory:
# the daemon spawns winwings-worker.exe from alongside itself.

GIT_HEAD = $(shell git rev-parse HEAD | head -c8)
LDFLAGS  = -X github.com/pterodactyl/wings/system.Version=$(GIT_HEAD)

.PHONY: all build release test clean

all: build

# Development build into ./build.
build:
	go build -ldflags="$(LDFLAGS)" -o build/wings.exe wings.go
	go build -ldflags="$(LDFLAGS)" -o build/winwings-worker.exe ./cmd/winwings-worker

# Stripped release build.
release:
	go build -trimpath -ldflags="-s -w $(LDFLAGS)" -o build/wings.exe wings.go
	go build -trimpath -ldflags="-s -w $(LDFLAGS)" -o build/winwings-worker.exe ./cmd/winwings-worker

test:
	go test ./...

# The Job Object, process and worker tests drive the real Windows kernel, so
# they only mean anything on a Windows host.
test-integration:
	go test ./internal/jobobject/ ./internal/winproc/ ./internal/worker/ ./internal/winfs/ -v

clean:
	rm -rf build
