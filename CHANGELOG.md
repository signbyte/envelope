# Changelog

Notable changes to this service, newest first, per release. This file is written for whoever
runs the service or integrates against it.

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
