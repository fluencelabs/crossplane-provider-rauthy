.PHONY: generate test test.envtest lint build image xpkg.build clean

# Every go command uses go.mod's own Go version, the one CI sets up.
GOTOOLCHAIN ?= go$(shell awk '$$1 == "go" { print $$2; exit }' go.mod)
export GOTOOLCHAIN

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORMS ?= linux/amd64 linux/arm64
IMAGE ?= crossplane-provider-rauthy
OUTPUT := _output
GO_LDFLAGS := -s -w -X github.com/fluencelabs/crossplane-provider-rauthy/internal/version.Version=$(VERSION)

DOCKER ?= docker
# The crossplane CLI builds the package; a 2.x release is required.
CROSSPLANE ?= crossplane

# Regenerate deepcopy methods, the crossplane-runtime method sets and the CRDs
# under package/crds. CI checks that this leaves no diff.
generate:
	go generate -tags generate ./apis/...

test:
	go test ./... -race -count=1

# The controller against a real kube-apiserver and etcd, fetched by
# setup-envtest for ENVTEST_K8S_VERSION into ENVTEST_DIR.
ENVTEST_K8S_VERSION ?= 1.35.0
SETUP_ENVTEST ?= sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.3
ENVTEST_DIR ?= $(OUTPUT)/envtest

test.envtest:
	assets="$$(go run $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(ENVTEST_DIR) -p path)" && \
		KUBEBUILDER_ASSETS="$$(cd "$$assets" && pwd)" \
		go test -tags envtest ./internal/controller/client/ -race -count=1 -run '^TestEnv'

lint:
	golangci-lint run

# The controller binary for the local platform.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(GO_LDFLAGS)" -o $(OUTPUT)/bin/provider ./cmd/provider

# One controller image per platform, saved as a tarball for xpkg.build.
image:
	@mkdir -p $(OUTPUT)/images
	@for p in $(PLATFORMS); do \
		arch=$${p#*/}; \
		echo "building $(IMAGE):$(VERSION)-$$arch"; \
		$(DOCKER) build --platform $$p --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION)-$$arch . || exit 1; \
		$(DOCKER) save -o $(OUTPUT)/images/$(IMAGE)-$$arch.tar $(IMAGE):$(VERSION)-$$arch || exit 1; \
	done

# One provider package per platform with its controller image embedded. Push
# them together with `crossplane xpkg push -f a.xpkg,b.xpkg REF` to publish a
# multi-platform package.
xpkg.build: image
	@mkdir -p $(OUTPUT)/xpkg
	@for p in $(PLATFORMS); do \
		arch=$${p#*/}; \
		$(CROSSPLANE) xpkg build \
			--package-root=package \
			--examples-root=examples \
			--embed-runtime-image-tarball=$(OUTPUT)/images/$(IMAGE)-$$arch.tar \
			--package-file=$(OUTPUT)/xpkg/$(IMAGE)-$$arch.xpkg || exit 1; \
	done
	@ls -l $(OUTPUT)/xpkg

clean:
	rm -rf $(OUTPUT)
