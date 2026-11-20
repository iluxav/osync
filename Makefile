VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64

.PHONY: build dist test test-wayland clean

# Binary for this machine, in the project root.
build:
	go build -ldflags '$(LDFLAGS)' -o osync .

# Binaries for every supported machine, in dist/. install.sh picks from there
# on machines without Go.
dist:
	@for t in $(TARGETS); do \
		echo "building dist/osync-$$t"; \
		CGO_ENABLED=0 GOOS=$${t%-*} GOARCH=$${t#*-} \
			go build -trimpath -ldflags '$(LDFLAGS)' -o dist/osync-$$t . || exit 1; \
	done

test:
	go vet ./...
	go test -race ./...

# Drives the real wl-clipboard tools in the running Wayland session. Uses the
# primary selection (middle-click buffer), not the clipboard.
test-wayland:
	OMARCHY_SYNC_TEST_WAYLAND=1 go test -count=1 -v -run TestWaylandSession ./internal/clipboard

clean:
	rm -rf dist osync
