# Changelog

Notable changes to this service, newest first, per release. This file is written for whoever
runs the service or integrates against it.

## v0.1.0

Initial code.

The Envelope/Workflow service as first released: the multi-document, multi-signer workflow
brain — owns envelopes and their signer slots, runs the envelope state machine, enforces
signing order, and keeps the durable record of who must sign what, in what order, and where
each slot stands. Every transition is applied atomically under optimistic concurrency on the
envelope version. The community edition carries a two-signer envelope by design.
AGPL-3.0-only.
