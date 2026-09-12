# Changelog

Notable changes to this service, newest first, per release. This file is written for whoever
runs the service or integrates against it.

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
So a person invited as `PNOLV-010180-15097` is matched when they arrive as `PNOLV-01018015097`, and
the other way round — which they previously were not.

```http
POST /api/v1/envelopes
Content-Type: application/json

{ "title": "contract", "slots": [ { "orderIndex": 1, "identityRef": "PNOLV-010180-15097" } ] }
```

```json
{ "slots": [ { "orderIndex": 1, "identityRef": "PNOLV-01018015097" } ] }
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
