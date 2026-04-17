OAPI_CODEGEN_VERSION    := v2.6.0
OPENAPI_TS_VERSION      := 7.6.0
DATAMODEL_CODEGEN_VER   := 0.28.5
DATAMODEL_CODEGEN_PY    := 3.12

GO_CLIENT       := mobius/api/client.gen.go
GO_CLI_COMMANDS := cmd/mobius/commands.gen.go
TS_SCHEMA       := typescript/src/api/schema.ts
PY_MODELS       := python/deepnoodle/mobius/_api/models.py

.PHONY: build-cli release-cli \
        generate generate-go generate-go-cli generate-ts generate-py generate-check \
        test test-go test-ts test-py \
        tools tools-go tools-ts

build-cli:
	go build -o bin/mobius ./cmd/mobius

release-cli:
	@if [ -z "$(VERSION)" ]; then echo "Usage: make release-cli VERSION=0.1.0"; exit 1; fi
	python3 ./scripts/release.py $(VERSION)

# Regenerate clients in all three languages from openapi.yaml.
generate: generate-go generate-go-cli generate-ts generate-py

generate-go:
	@echo "=> generating Go client"
	oapi-codegen --config mobius/api/oapi-codegen.yaml openapi.yaml

# Regenerate mobius CLI subcommands from the Go client + OpenAPI spec.
# Override or suppress individual commands via internal/cligen/overrides.go.
generate-go-cli:
	@echo "=> generating Go CLI commands"
	go run ./internal/cligen \
		--client $(GO_CLIENT) \
		--spec openapi.yaml \
		--out-dir cmd/mobius

generate-ts:
	@echo "=> generating TypeScript schema"
	@if [ ! -d typescript/node_modules ]; then $(MAKE) tools-ts; fi
	cd typescript && pnpm --silent generate

generate-py:
	@echo "=> generating Python models"
	@mkdir -p python/deepnoodle/mobius/_api
	@touch python/deepnoodle/mobius/_api/__init__.py
	uvx --python $(DATAMODEL_CODEGEN_PY) \
		--from 'datamodel-code-generator[http]==$(DATAMODEL_CODEGEN_VER)' \
		datamodel-codegen \
		--input openapi.yaml \
		--input-file-type openapi \
		--output $(PY_MODELS) \
		--output-model-type pydantic_v2.BaseModel \
		--target-python-version 3.11 \
		--use-standard-collections \
		--use-union-operator \
		--field-constraints \
		--use-schema-description \
		--disable-timestamp

# CI check: ensure all generated clients match the committed spec.
generate-check: generate
	@changed=$$(git diff --name-only -- $(GO_CLIENT) $(GO_CLI_COMMANDS) $(TS_SCHEMA) $(PY_MODELS)); \
	if [ -n "$$changed" ]; then \
		git diff -- $(GO_CLIENT) $(GO_CLI_COMMANDS) $(TS_SCHEMA) $(PY_MODELS); \
		echo; \
		echo "ERROR: generated clients are out of date: $$changed"; \
		echo "Run 'make generate' and commit the result."; \
		exit 1; \
	fi

# Run all three SDK test suites. Cross-language contract tests in each
# language exercise the shared fixtures under internal/testdata/contract, so
# this target is the single check that proves wire-format parity.
test: test-go test-ts test-py

test-go:
	@echo "=> go test"
	go test ./...

test-ts:
	@echo "=> typescript test"
	@if [ ! -d typescript/node_modules ]; then $(MAKE) tools-ts; fi
	cd typescript && pnpm --silent test

test-py:
	@echo "=> python test"
	cd python && uv run --python $(DATAMODEL_CODEGEN_PY) pytest tests/

tools: tools-go tools-ts
	@echo "Python codegen uses uvx — no persistent install needed."
	@echo "Ensure 'uv' is on your PATH (https://docs.astral.sh/uv/)."

tools-go:
	go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)

tools-ts:
	cd typescript && pnpm install --frozen-lockfile
