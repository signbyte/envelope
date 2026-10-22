# Changelog

Notable changes to this service, newest first, per release. This file is written for whoever
runs the service or integrates against it.

## v0.4.1

A security release. A token exchanged on a caller's behalf is no longer reused for a different token of the same
person, and **the service now builds with Go 1.27.3**. Nothing about the API, the configuration or the database
changes.

### Security

- **The authentication library moves to `go-authbyte` v0.31.1.** It caches a token exchanged on a caller's behalf
  under the caller's own token, not under the person alone, and stops reusing it once that token has expired.
  Before, a person whose token was renewed with different claims (another organisation, a stronger sign-in) could
  have this service's onward calls carried for a few minutes by the token exchanged for the earlier one. Nothing to
  configure; one more exchange per renewed token.
- **Go 1.27.3** (was 1.27.2), in `go.mod` and as the image's default build toolchain (`ARG GO_VERSION` in the
  `Dockerfile`). Go 1.27.3 fixes GO-2026-6809 in `crypto/x509`, which code making TLS calls reaches. **Rebuild the
  image to pick the fix up.**

### Changed

- **Libraries:** azugo → v0.40.1, `fasthttp` → v1.76.0, `valkey-go` → v1.0.79, `go-jose` → v4.1.6,
  plus their indirect modules.

## v0.4.0

### Changed — a signer slot names the CSC flow by how the card is read

A slot's `flow` accepts `cscEidScan` and `cscEidPlugin` in place of `csc`, which is now refused. Slots
already stored with `csc` are read back as they are.

```http
POST /api/v1/envelopes
{ "title": "…", "slots": [ { "orderIndex": 1, "flow": "cscEidPlugin" } ] }
```

Needs the database migration that knows the two names (the platform database's 2026-09-28 entry).

### Changed — the service builds with Go 1.27.2, on its libraries' Go 1.27.2 releases

A security change. Nothing about the API, the configuration or the database moves with it.

- **Go 1.27.2** (was 1.27.0), both in `go.mod` and as the image's default build toolchain
  (`ARG GO_VERSION` in the `Dockerfile`). Go 1.27.2 and `golang.org/x/net` v0.60.0 fix
  vulnerabilities that this service's code reaches under 1.27.0: `govulncheck` found nine it calls,
  GO-2026-6603, -6605, -6607, -6608, -6610, -6611, -6612, -6613 and -6617, in `net/http`,
  `crypto/tls`, `net/textproto` and `golang.org/x/net`. Under the new set it finds none. **Rebuild
  the image to pick the fixes up**; an image built before this change still carries them.
- **Libraries:** `go-authbyte` → v0.29.1 (from v0.23.1), `go-platform-kit` → v1.11.4,
  `go-sec-events` → v1.2.2, `go-gdpr-audit` → v1.1.6, azugo → v0.40.0, `fasthttp` → v1.75.0, plus 41
  indirect modules with them. The go-authbyte releases in between add features this service does not
  use, with one visible change: when the authorization server refuses a token request, the error no
  longer repeats the server's answer, so a log line built from that error stops carrying it (the
  message is now `the token call responded <status>`). None of azugo v0.40.0's behaviour changes
  reaches this service: it sets no cookies, issues no redirects after a non-GET request, makes no
  outbound calls through azugo's HTTP client and implements no azugo cache.

### Notes

- The image build now caches Go modules and compiled packages between builds. The binary is built by
  the same command with the same flags; only the build's layer order and caches changed.
- CI's linter moved to golangci-lint v2.14.0. The earlier release cannot read Go 1.27.2's compiled
  standard library and stops before linting anything.
- The gate is green on Go 1.27.2: `go mod verify`, `go mod tidy -diff`, build, vet, `gofmt`,
  golangci-lint v2.14.0, `go test -race` with **0 races**; `govulncheck` reports **no
  vulnerabilities found**.

## v0.3.1

### Changed — the metrics endpoint no longer offers OpenMetrics

A scraper that asked for the OpenMetrics format by sending `Accept: application/openmetrics-text`
used to be answered in it, with the `# EOF` terminator that format requires. This service now
answers in the Prometheus text format whatever the scraper asks for, and writes no `# EOF`:

```http
GET /metrics
Accept: application/openmetrics-text

200 OK
Content-Type: text/plain; version=0.0.4; charset=utf-8
```

**The metric names, labels and values are unchanged**, so Prometheus — and anything else that
accepts the plain-text exposition format — needs nothing done. Two setups need a look: a scrape
configuration that *requires* the OpenMetrics content type, and a check that reads a missing
`# EOF` as a truncated scrape. Both need their expectation relaxed.

The endpoint itself is unchanged otherwise: still `/metrics` (or `METRICS_PATH`), still enabled by
default, and still answered only for trusted addresses (`METRICS_TRUSTED_IPS`, `127.0.0.1` by
default) — so if nothing scrapes this service, there is nothing to do. The change arrives from the
web framework this service is built on rather than from a change of its own, carried in with the
shared libraries below.

### Changed — a signer slot's identity code is stored in one spelling, and a bare code is refused

An `identityRef` on a slot is now **rewritten to one canonical spelling** before it is stored: the
identity type, the country, a hyphen, and the national code with its separators removed. The
authenticated caller's identity code is reduced the same way before it is matched against a slot.
So a person invited as `PNOLV-XXXXXX-XXXXX` is matched when they arrive as `PNOLV-XXXXXXXXXXX`, and
the other way round — which they previously were not.

```http
POST /api/v1/envelopes
Content-Type: application/json

{ "title": "contract", "slots": [ { "orderIndex": 1, "identityRef": "PNOLV-XXXXXX-XXXXX" } ] }
```

```json
{ "slots": [ { "orderIndex": 1, "identityRef": "PNOLV-XXXXXXXXXXX" } ] }
```

**A code that names no country is now refused** — `422`, naming the field, repeating no identity
code back:

```json
{
  "title": "Unprocessable entity",
  "status": 422,
  "detail": "Key: 'slots[0].identityRef' Error:Field validation for 'slots[0].identityRef' failed on the 'identityRef' tag",
  "code": "err:request:unprocessable"
}
```

This service is nowhere near the person and will not guess their country: the same eleven digits
belong to a different person in a different country, and a wrong identity key is the wrong person's
documents. Whoever is close enough to know — the screen the code was typed on, the certificate it
was read from, the register of the system that sent it — supplies it. **A caller that sends bare
national codes must start sending qualified ones** (`PNOLV-…`). Both write paths apply the rule:
`POST /api/v1/envelopes` and `POST /api/v1/envelopes/{id}/slots`.

### Notes

- The shared libraries moved to their current releases — the auth client at v0.21.0 and the
  platform kit at v1.11.2 — which carried the web framework, the HTTP stack and the JOSE library up
  with them. No endpoint, field, error or setting of this service changed, and no configuration
  needs touching. The move also clears two published advisories in the cryptography library this
  service depends on; a third has no fix available yet and was already present before the move, and
  the vulnerability scanner reports nothing this service's own code can reach.

### Changed — the shared libraries move to their current releases

`go-platform-kit` v1.11.3, `go-authbyte` v0.23.1, `go-gdpr-audit` v1.1.5 and `go-sec-events`
v1.2.1. No endpoint, field, error or setting changes with them, nothing in your configuration needs
touching, and this service's own behaviour is unchanged. `go-authbyte` crosses v0.23.0 on the way,
which adds a way to tell a natural person's identity code from an organisation's — an addition to
the library, not a change to anything this service does. The Postgres driver `pgx/v5` moves to
v5.11.0 in the same pass.

## v0.3.0

### Added — an envelope can say where it came from, and each signer where to go back

A document system can prepare an envelope for its own user and hand the signer to the portal by
link. `POST /api/v1/envelopes` accepts an optional `origin` object — `name` (required within it: the
requesting system's registered display name), `returnUrl` (the default address the signer's browser
is offered afterwards) and `ref` (the requester's own reference) — and every slot, in the create body
or through `POST /api/v1/envelopes/{id}/slots`, accepts an optional `returnUrl` overriding that
default. `GET /api/v1/envelopes/{id}` returns `envelope.origin` (absent when none was recorded) and
`slots[].returnUrl`. Nothing changes for an envelope started without an origin.

```http
POST /api/v1/envelopes
{ "title": "Delivery contract", "orderPolicy": "sequential",
  "origin": { "name": "Acme DMS", "returnUrl": "https://dms.example/return", "ref": "contracts/2026-117" },
  "slots": [ { "orderIndex": 1, "returnUrl": "https://dms.example/contracts/2026-117" },
             { "orderIndex": 2 } ] }
```

A return address is admitted only as an absolute https URL without credentials or a fragment; anything
else is refused with `422` before it is stored, because a stored address is later offered to a person's
browser as a place to go. Whether a destination is *registered* for the requester is the calling
service's check, made against its own registry before the call — this service admits the shape.

**Deployment note:** needs the platform database migration that adds the columns (`envelope` location
`V7`); apply it before or with this version. Against an older database the origin keys are dropped
silently by the procedures, so the order matters.

## v0.2.0

### Changed — the envelope listing excludes expired envelopes

`GET /api/v1/envelopes` no longer serves envelopes whose retention has expired them
(`"status": "expired"`). Once retention closes an envelope its home is the durable record:
serving it in the live listing renders a workflow nobody can act on, typically over
already-destroyed document storage. The by-id read (`GET /api/v1/envelopes/{id}`) is
unchanged and keeps answering for an expired envelope throughout its keep window, so
tracking links and history views still resolve.

A consumer that needs expired envelopes listed can ask the data layer directly — the
underlying procedure takes an `include_expired` flag — but this service deliberately does
not expose it on the API.

**Deployment note:** the behaviour lives in the platform database's `envelope.list_envelopes`
procedure and arrives by applying the platform database migration; this service's binary is
unchanged. Deployments applying migrations on every rollout get it automatically.

## v0.1.0

Initial code.

The Envelope/Workflow service as first released: the multi-document, multi-signer workflow
brain — owns envelopes and their signer slots, runs the envelope state machine, enforces
signing order, and keeps the durable record of who must sign what, in what order, and where
each slot stands. Every transition is applied atomically under optimistic concurrency on the
envelope version. The community edition carries a two-signer envelope by design.
AGPL-3.0-only.
