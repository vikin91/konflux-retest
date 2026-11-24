.PHONY: build
build:
	version=$$(git describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --always --dirty 2>/dev/null || printf '%s' dev); \
	go build -ldflags "-X main.version=$$version" -o ./konflux-retest ./cmd/konflux-retest
