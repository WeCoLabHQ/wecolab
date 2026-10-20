VERSION ?= dev
LDFLAGS := -s -w -X main.Version=$(VERSION)

.PHONY: test vet build dist crds join dev-up dev-test dev-down

test: vet join
	go test ./...

vet:
	go vet ./...

build:
	@flags="$$(python3 hack/build-info.py --format go-ldflags)" || exit 1; \
	  go build -ldflags "$(LDFLAGS) $$flags" -o bin/ ./cmd/...

# Linux binaries for both architectures: what install.sh images when WECOLAB_BIN points here.
dist: join
	@mkdir -p dist/bin
	@flags="$$(python3 hack/build-info.py --format go-ldflags)" || exit 1; \
	  for a in amd64 arm64; do for c in warden console; do \
	    CGO_ENABLED=0 GOOS=linux GOARCH=$$a go build -trimpath -ldflags "$(LDFLAGS) $$flags" -o dist/bin/$$c-$$a ./cmd/$$c || exit 1; done; done
	@echo $(VERSION) > dist/bin/VERSION

crds:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0 object paths=./api/... crd:crdVersions=v1 output:crd:dir=internal/bootstrap/template/crds

# The Console serves install.sh as join.sh; the copy is embedded.
join:
	@cmp -s install.sh cmd/console/join.sh || cp install.sh cmd/console/join.sh

dev-up:
	hack/dev/fabric.sh up
dev-test:
	hack/dev/fabric.sh test
dev-down:
	hack/dev/fabric.sh down
