# DockBack — build, test, and security-scan gates (Security.md §2/§5, PLAN §10.2).
#
# `make scan` runs the full gate: image CVEs (Trivy + Grype), Dockerfile lint
# (Hadolint), Go vulns (govulncheck), frontend prod-dep advisories (npm audit),
# and writes an SBOM (Syft). External scanners run via their official Docker
# images (only Docker is required) and scan a `docker save` tar — no Docker
# socket is mounted into the scanners.

IMAGE   ?= dockback:latest
VERSION ?= dev
DIST    ?= dist
IMG_TAR := $(DIST)/image.tar
SBOM    := $(DIST)/sbom.spdx.json

# Pin these for reproducibility in CI (defaults track latest).
TRIVY_IMAGE    ?= aquasec/trivy:latest
GRYPE_IMAGE    ?= anchore/grype:latest
SYFT_IMAGE     ?= anchore/syft:latest
HADOLINT_IMAGE ?= hadolint/hadolint:latest
GOVULN         ?= golang.org/x/vuln/cmd/govulncheck@latest

DOCKER_RUN := docker run --rm

# Supply-chain signing (PLAN §10.3). `sign` signs a pushed digest and
# `verify-image` verifies one. Keyless signing needs an OIDC identity, so it
# only works from GitHub Actions (release.yml) — and `.github/` is not in this
# repo's tracked file set, so nothing signs releases today. Publishing that
# workflow is what turns these targets on; COSIGN_IDENTITY is the identity it
# would sign as.
COSIGN_IDENTITY ?= https://github.com/.*/.github/workflows/release.yml@refs/tags/.*
COSIGN_ISSUER   ?= https://token.actions.githubusercontent.com
IMAGE_REF       ?= $(IMAGE)

.PHONY: all build test scan scan-dockerfile scan-go scan-npm scan-image sbom save ci clean e2e sign verify-image

all: build

## build: build the app image (no compose / .env needed — CI-friendly).
build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

## test: frontend tests + build, then Go unit tests.
test:
	sh scripts/check-recover-copy.sh
	# The UI is built FIRST: internal/web embeds its output, so in a fresh
	# clone (where that output is not committed) the Go packages do not
	# compile until it exists.
	cd frontend && npm ci && npm run test && npm run build
	# -tags testhooks: the audit tamper helpers are excluded from the shipped
	# binary and compiled only for the tests that prove the beacon catches a
	# rewritten trail. Without the tag those tests do not build.
	GOFLAGS=-mod=mod go test -tags testhooks ./...

# Save the current image to a tar so the scanners can read it socket-free.
save:
	@mkdir -p $(DIST)
	docker save $(IMAGE) -o $(IMG_TAR)

## scan: run every security gate; any finding fails the build.
scan: scan-dockerfile scan-go scan-npm scan-image sbom
	@echo "✓ all security scans passed"

## scan-dockerfile: Hadolint (fails on error-level findings).
scan-dockerfile:
	@echo "▶ hadolint (Dockerfile)"
	$(DOCKER_RUN) -i $(HADOLINT_IMAGE) hadolint --failure-threshold error - < Dockerfile

## scan-go: govulncheck — reachable vulns in Go modules (allowlist for no-fix advisories).
scan-go:
	@echo "▶ govulncheck (Go)"
	sh scripts/govulncheck.sh

## scan-npm: npm audit on production frontend deps (high+ fails).
scan-npm:
	@echo "▶ npm audit (frontend, prod deps)"
	cd frontend && npm audit --omit=dev --audit-level=high

## scan-image: Trivy + Grype CVE scan of the built image (HIGH/CRITICAL, fixable).
scan-image: save
	@echo "▶ trivy (image CVEs)"
	$(DOCKER_RUN) -v $(PWD)/$(DIST):/dist -v $(PWD)/.trivyignore:/.trivyignore:ro $(TRIVY_IMAGE) image \
		--input /dist/$(notdir $(IMG_TAR)) --ignorefile /.trivyignore \
		--exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed
	@echo "▶ grype (image CVEs)"
	$(DOCKER_RUN) -v $(PWD)/$(DIST):/dist -v $(PWD)/.grype.yaml:/.grype.yaml:ro $(GRYPE_IMAGE) \
		--config /.grype.yaml docker-archive:/dist/$(notdir $(IMG_TAR)) --fail-on high --only-fixed

## sbom: write an SPDX SBOM with Syft.
sbom: save
	@echo "▶ syft (SBOM → $(SBOM))"
	@mkdir -p $(DIST)
	$(DOCKER_RUN) -v $(PWD)/$(DIST):/dist $(SYFT_IMAGE) \
		docker-archive:/dist/$(notdir $(IMG_TAR)) -o spdx-json=/dist/$(notdir $(SBOM))
	@echo "  wrote $(SBOM)"

## e2e: end-to-end DR drill — backup -> verify -> restore on a throwaway stack (§9.4).
e2e:
	bash scripts/e2e.sh

## sign: cosign keyless-sign a pushed image by digest (run in CI with OIDC).
sign:
	cosign sign --yes $(IMAGE_REF)

## verify-image: verify a published image's cosign signature + identity (consumers).
verify-image:
	cosign verify $(IMAGE_REF) \
		--certificate-identity-regexp '$(COSIGN_IDENTITY)' \
		--certificate-oidc-issuer '$(COSIGN_ISSUER)'

## ci: full pipeline — test, build, scan.
ci: test build scan

## clean: remove scan artifacts.
clean:
	rm -rf $(DIST)
