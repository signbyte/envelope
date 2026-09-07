# Changelog

Notable changes to this service, newest first, per release. This file is written for whoever
runs the service or integrates against it.

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
