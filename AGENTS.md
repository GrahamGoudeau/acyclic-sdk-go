# Working in acyclic-sdk-go

- Standard library only, in the module and in tests. Do not add a `require` line to `go.mod`. The `go` directive stays at 1.22.
- One concept per file, mirroring acyclic-sdk-js `src/`: `auth.go`, `fhir.go`, `vendors.go`, `tools.go`, `projections.go`, `agent.go`, `models.go`, `sigv4.go`, `telemetry.go`, `store.go`. The JS SDK is the reference; behavior changes land there and in the spec first.
- `spec/` is a copy of acyclic-sdk-spec. Change it there and copy it over. `spec/tools.json` is embedded with `go:embed`.
- Projections must match `spec/fixtures/projections` exactly (`TestProjectionFixtures`). Add a fixture when you change a projection rule. `value_number` stays a JSON number; absent fields are omitted.
- Telemetry: every attribute goes through the allow-list in `telemetry.go` inside the SDK (`guard`). Never bypass it, never widen it without changing `spec/telemetry.md`, and never put query strings, bodies, resource ids, prompts, tool arguments or results, tokens or keys into spans, logs or error messages. Wrap `*url.Error` (it contains the URL) before returning it.
- Vendor differences live in `vendors.go` only. Do not special-case vendors anywhere else.
- Only GET requests retry. Credentials go only to the FHIR base origin.
- Tests:
  ```sh
  go vet ./... && go test ./...
  SANDBOX_URL=https://sandbox.acyclic.sh go test ./...   # or a local acyclic-sandbox: PORT=8190 go run ./cmd/sandbox
  ```
  The end-to-end test registers an app per vendor (epic, oracle, athena, meditech, ecw), runs every tool and asserts telemetry never contains the patient's name, birth date or raw id.
