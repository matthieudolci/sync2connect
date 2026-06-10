VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o sync2connect ./cmd/sync2connect

.PHONY: test
test:
	go test ./... -race -count=1

.PHONY: lint
lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:" && gofmt -l . && exit 1)

.PHONY: cross
cross:
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
		echo "building dist/sync2connect-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
			-ldflags="$(LDFLAGS)" \
			-o dist/sync2connect-$$os-$$arch$$ext ./cmd/sync2connect || exit 1; \
	done

.PHONY: docker
docker:
	docker build --build-arg VERSION=$(VERSION) -t sync2connect:$(VERSION) .

.PHONY: clean
clean:
	rm -rf dist sync2connect sync2connect.exe
