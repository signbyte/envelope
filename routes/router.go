// Package routes registers the envelope/workflow service HTTP API.
package routes

import (
	envelope "github.com/signbyte/envelope"
)

type router struct {
	*envelope.App
}

// Init registers all routes. The inbound API is DPoP-gated (go-authbyte); callers
// are the backend-for-frontend (the user's delegated actions) and the Signing
// Orchestrator (the slot-signed callback). Owner-side reads/writes are owner-filtered
// on the caller subject; the signer-side surface (the signer inbox + per-slot read/act)
// additionally grants an invited signer matched on their authenticated identity.
func Init(a *envelope.App) error {
	r := &router{App: a}

	// Public liveness/readiness.
	a.Get("/healthz", r.healthz)
	a.Get("/readyz", r.readyz)

	// Authenticated API.
	v1 := a.Group("/api/v1")
	v1.Use(a.AuthMiddleware())

	// The signer inbox — envelopes awaiting the caller's signature (an invited signer,
	// not the owner). Keyed on the caller's authenticated identity, not ownership.
	v1.Get("/signing-tasks", r.listSigningTasks)

	// Envelope lifecycle + composition.
	v1.Post("/envelopes", r.createEnvelope)
	v1.Get("/envelopes", r.listEnvelopes)
	v1.Get("/envelopes/{id}", r.getEnvelope)
	v1.Post("/envelopes/{id}/documents", r.attachDocument)
	v1.Post("/envelopes/{id}/slots", r.addSlot)
	v1.Post("/envelopes/{id}/send", r.sendEnvelope)
	v1.Post("/envelopes/{id}/cancel", r.cancelEnvelope)
	v1.Post("/envelopes/{id}/reopen", r.reopenEnvelope)

	// Per-slot ordering + the signing linkage.
	v1.Get("/envelopes/{id}/slots/{slot}/eligible", r.slotEligible)
	v1.Post("/envelopes/{id}/slots/{slot}/job", r.setSlotJob)
	v1.Post("/envelopes/{id}/slots/{slot}/signed", r.slotSigned)
	v1.Post("/envelopes/{id}/slots/{slot}/decline", r.declineSlot)
	v1.Post("/envelopes/{id}/slots/{slot}/name", r.captureSignerName)

	return nil
}
