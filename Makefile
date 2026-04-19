# Build, test and quality gates.
#
# Tools install into ./.tools (gitignored) at pinned-by-latest rather than being
# expected on PATH, so a fresh clone and CI run the same versions of the same
# checks. `make tools` is the only target that touches the network.

TOOLS   := $(CURDIR)/.tools
GO      ?= go
PKGS    := ./...

# gosec rules excluded because the flagged behaviour IS the product. Anything it
# finds that is genuinely a defect gets fixed instead of listed -- the three on
# its first run (two 0755 directories, one holding the credential signing key,
# and a proxy server with no ReadHeaderTimeout) were all real and are all fixed.
#
#   G204  Forking a configured command is what this program does. The supervisor
#         runs whatever -session-cmd names, the local driver runs the supervisor
#         binary, and sessionuid re-execs this binary to probe a write. None of
#         those arguments come from a session.
#   G304  The explorer reads caller-supplied paths; that is its purpose.
#         Containment is enforced by internal/supervisor/fsjail.go, which is
#         tested, rather than by never accepting a path.
#   G704  The gateway broker forwards to an operator-configured upstream. The
#         control that matters there is the route allowlist, not the hostname.
#   G115  Terminal rows/cols are uint16 by the ioctl's definition and uids are
#         uint32 by the syscall's. The conversions sit at those boundaries.
#   G101  gateway.PlaceholderToken is deliberately not a secret -- it is the
#         obviously-fake value a session gets so nothing real is in its
#         environment, named to be recognisable in a log or a paste.
GOSEC_EXCLUDE := G204,G304,G704,G115,G101

.PHONY: all
all: lint test

.PHONY: build
build:
	$(GO) build -o bin/ ./cmd/...

# -race everywhere: this codebase is goroutines around a PTY and a tunnel, and
# the bugs it has actually hit (a detach deadlock, a sampler outliving its test)
# are exactly the kind -race finds.
#
# -p 1 because four packages now start a Postgres container through
# testcontainers. Running them concurrently once cascaded into 16 failures that
# a serial rerun did not reproduce -- container contention, reported as
# unrelated assertion failures, which is the worst way for it to present.
.PHONY: test
test:
	$(GO) test $(PKGS) -race -p 1

# Regenerate the typed query methods for both tiers. Not part of `all`: it is a
# deliberate step after editing a .sql file, and the generated output is
# committed so a clone builds without the tool.
.PHONY: sqlc
sqlc: $(TOOLS)/sqlc
	$(TOOLS)/sqlc generate

.PHONY: fmt
fmt:
	$(GO) fmt $(PKGS)
	cd terraform && terraform fmt

.PHONY: lint
lint: $(TOOLS)/golangci-lint
	$(TOOLS)/golangci-lint run $(PKGS)

.PHONY: sec
sec: $(TOOLS)/gosec $(TOOLS)/govulncheck
	$(TOOLS)/gosec -quiet -exclude=$(GOSEC_EXCLUDE) -exclude-dir=.tools $(PKGS)
	$(TOOLS)/govulncheck $(PKGS)

# Scoped deliberately. Gremlins is slow on large modules and still 0.x with no
# compatibility guarantee, so it runs where a surviving mutant means something:
# credential derivation and, once it exists, authorisation.
.PHONY: mutation
mutation: $(TOOLS)/gremlins
	$(TOOLS)/gremlins unleash ./internal/creds

.PHONY: check-oss-tree
check-oss-tree:
	bash scripts/check-oss-tree.sh

.PHONY: shellcheck
shellcheck:
	@command -v shellcheck >/dev/null || { echo "shellcheck not installed: brew install shellcheck"; exit 1; }
	shellcheck image/*.sh

.PHONY: console-check
console-check:
	cd console && bun run typecheck && bunx oxlint

.PHONY: ci
ci: lint test sec

.PHONY: tools
tools: $(TOOLS)/golangci-lint $(TOOLS)/gosec $(TOOLS)/govulncheck $(TOOLS)/gremlins $(TOOLS)/sqlc

$(TOOLS)/sqlc:
	GOBIN=$(TOOLS) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@latest

$(TOOLS)/golangci-lint:
	GOBIN=$(TOOLS) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

$(TOOLS)/gosec:
	GOBIN=$(TOOLS) $(GO) install github.com/securego/gosec/v2/cmd/gosec@latest

$(TOOLS)/govulncheck:
	GOBIN=$(TOOLS) $(GO) install golang.org/x/vuln/cmd/govulncheck@latest

$(TOOLS)/gremlins:
	GOBIN=$(TOOLS) $(GO) install github.com/go-gremlins/gremlins/cmd/gremlins@latest

.PHONY: clean
clean:
	rm -rf bin $(TOOLS)
