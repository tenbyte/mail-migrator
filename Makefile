GO ?= go
WAILS_VERSION := v2.15.0
GOVULNCHECK_VERSION := v1.7.0
CYCLONEDX_VERSION := v1.12.0

.PHONY: dev test test-race lint audit sbom frontend build-macos build-windows clean

dev:
	GOTOOLCHAIN=go1.27.0 $(GO) run github.com/wailsapp/wails/v2/cmd/wails@$(WAILS_VERSION) dev

frontend:
	cd frontend && npm ci && npm run build

test: frontend
	GOTOOLCHAIN=go1.27.0 $(GO) test . ./internal/...
	cd frontend && npm test

test-race: frontend
	GOTOOLCHAIN=go1.27.0 $(GO) test -race . ./internal/...

lint: frontend
	GOTOOLCHAIN=go1.27.0 $(GO) vet . ./internal/...
	cd frontend && npm run lint

audit: frontend
	GOTOOLCHAIN=go1.27.0 $(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) . ./internal/...
	cd frontend && npm audit

sbom: frontend
	mkdir -p build/compliance
	GOTOOLCHAIN=go1.27.0 $(GO) run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_VERSION) app -json -output build/compliance/go-sbom.cdx.json
	cd frontend && npm sbom --sbom-format cyclonedx > ../build/compliance/frontend-sbom.cdx.json

build-macos: frontend
	MACOSX_DEPLOYMENT_TARGET=13.0 CGO_CFLAGS=-mmacosx-version-min=13.0 CGO_LDFLAGS=-mmacosx-version-min=13.0 GOTOOLCHAIN=go1.27.0 $(GO) run github.com/wailsapp/wails/v2/cmd/wails@$(WAILS_VERSION) build -platform darwin/universal -clean

build-windows: frontend
	GOTOOLCHAIN=go1.27.0 $(GO) run github.com/wailsapp/wails/v2/cmd/wails@$(WAILS_VERSION) build -platform windows/amd64 -clean

clean:
	$(RM) -r build/bin frontend/dist
