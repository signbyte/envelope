package routes

import (
	"time"

	"azugo.io/azugo"

	"github.com/signbyte/envelope/store"
)

// slotInput is one signer slot in a create request or the add-slot body.
type slotInput struct {
	OrderIndex  int    `json:"orderIndex"`
	Role        string `json:"role" validate:"omitempty,oneof=signer approver observer"`
	Flow        string `json:"flow" validate:"omitempty,oneof=webEid eidScan eparakstsMobile eparakstsMobileEseal csc"`
	RequiredLoA string `json:"requiredLoa"`
	IdentityRef string `json:"identityRef"`
}

// createEnvelopeRequest is the body of POST /api/v1/envelopes. The owner is the
// authenticated caller (never taken from the body). Initial documents + slots are
// optional; documents are validated on behalf of the user and their content hash
// pinned, exactly as the dedicated attach endpoint does.
type createEnvelopeRequest struct {
	Title       string      `json:"title"`
	OrderPolicy string      `json:"orderPolicy" validate:"omitempty,oneof=parallel sequential"`
	Profile     string      `json:"profile"`
	Documents   []string    `json:"documents" validate:"omitempty,dive,required"`
	Slots       []slotInput `json:"slots" validate:"omitempty,dive"`
	// ExpiresAt is the deadline by which the invited parties are expected to sign
	// (RFC 3339). Omitted, the service applies its configured default; once it
	// passes, the envelope closes as expired on its own.
	ExpiresAt string `json:"expiresAt" validate:"omitempty,datetime=2006-01-02T15:04:05Z07:00"`
}

// Validate implements azugo.Validator (ctx.Body.JSON auto-validates).
func (r *createEnvelopeRequest) Validate(ctx *azugo.Context) error {
	return ctx.Validate().Struct(r)
}

// attachDocumentRequest is the body of POST /api/v1/envelopes/{id}/documents.
type attachDocumentRequest struct {
	DocumentID string `json:"documentId" validate:"required"`
}

// Validate implements azugo.Validator.
func (r *attachDocumentRequest) Validate(ctx *azugo.Context) error {
	return ctx.Validate().Struct(r)
}

// addSlotRequest is the body of POST /api/v1/envelopes/{id}/slots.
type addSlotRequest struct {
	slotInput
}

// Validate implements azugo.Validator.
func (r *addSlotRequest) Validate(ctx *azugo.Context) error {
	return ctx.Validate().Struct(r)
}

// setSlotJobRequest is the body of POST /api/v1/envelopes/{id}/slots/{slot}/job:
// record the signing job id on a slot when signing starts.
type setSlotJobRequest struct {
	JobID string `json:"jobId" validate:"required"`
}

// Validate implements azugo.Validator.
func (r *setSlotJobRequest) Validate(ctx *azugo.Context) error {
	return ctx.Validate().Struct(r)
}

// captureSignerNameRequest is the body of POST /api/v1/envelopes/{id}/slots/{slot}/name:
// the participant's display name, supplied by the portal from the caller's authenticated
// session (never client-typed on behalf of someone else).
type captureSignerNameRequest struct {
	Name string `json:"name" validate:"required,max=200"`
}

// Validate implements azugo.Validator.
func (r *captureSignerNameRequest) Validate(ctx *azugo.Context) error {
	return ctx.Validate().Struct(r)
}

// slotSignedRequest is the body of POST /api/v1/envelopes/{id}/slots/{slot}/signed:
// the signing service reports a slot finalized. signatureId is required; the
// signed document reference and job id are recorded when present.
type slotSignedRequest struct {
	SignatureID  string `json:"signatureId" validate:"required"`
	SignedDocRef string `json:"signedDocRef"`
	JobID        string `json:"jobId"`
}

// Validate implements azugo.Validator.
func (r *slotSignedRequest) Validate(ctx *azugo.Context) error {
	return ctx.Validate().Struct(r)
}

// createEnvelopeResponse is the result of creating an envelope: the new id, its
// status and version, and the ids of any slots created with it.
type createEnvelopeResponse struct {
	ID      string   `json:"id"`
	Status  string   `json:"status"`
	Version int      `json:"version"`
	SlotIDs []string `json:"slotIds,omitempty"`
}

// envelopeHeaderView is the envelope header in a read response. CreatedAt is the
// envelope's creation time (RFC3339, UTC), so the tracking page can show when it began.
type envelopeHeaderView struct {
	ID          string `json:"id"`
	Owner       string `json:"owner,omitempty"`
	TenantID    string `json:"tenantId,omitempty"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status"`
	OrderPolicy string `json:"orderPolicy"`
	Profile     string `json:"profile,omitempty"`
	Version     int    `json:"version"`
	CreatedAt   string `json:"createdAt,omitempty"`
}

// slotView is one signer slot in a read response, including the linkage to the
// signing job/signature fulfilling it. SignedAt orders the signings, so a reader can
// find the most-recently-produced container (the head of the co-sign chain).
type slotView struct {
	ID           string `json:"id"`
	OrderIndex   int    `json:"orderIndex"`
	IdentityRef  string `json:"identityRef,omitempty"`
	Role         string `json:"role"`
	Flow         string `json:"flow,omitempty"`
	RequiredLoA  string `json:"requiredLoa,omitempty"`
	Status       string `json:"status"`
	JobID        string `json:"jobId,omitempty"`
	SignatureID  string `json:"signatureId,omitempty"`
	SignedDocRef string `json:"signedDocRef,omitempty"`
	SignedAt     string `json:"signedAt,omitempty"`
	SignerName   string `json:"signerName,omitempty"`
}

// docRefView is one attached document reference in a read response.
type docRefView struct {
	DocumentID  string `json:"documentId"`
	ContentHash string `json:"contentHash"`
}

// envelopeView is the read of one envelope plus its slots and document references.
type envelopeView struct {
	Envelope  envelopeHeaderView `json:"envelope"`
	Slots     []slotView         `json:"slots"`
	Documents []docRefView       `json:"documents"`
}

// summaryView is one envelope in a listing. CreatedAt (RFC3339, UTC) drives the
// dashboard's "updated" column; SlotCount/SignedCount and YourTurn drive the owned-row
// progress badge (whether it is the owner's turn to sign one of their own slots).
type summaryView struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status"`
	OrderPolicy string `json:"orderPolicy"`
	Version     int    `json:"version"`
	CreatedAt   string `json:"createdAt,omitempty"`
	// UpdatedAt is the envelope's last mutation (RFC3339, UTC) — send, a slot
	// signing, decline, completion — so a dashboard can order by last action.
	UpdatedAt string `json:"updatedAt,omitempty"`
	// DocIDs are the envelope's attached document ids, so a listing consumer
	// can relate an envelope to the documents it covers without fetching each
	// envelope's detail view. Empty on the signer-inbox shape (a signer learns
	// the documents from the envelope view, not the task list).
	DocIDs      []string `json:"docIds,omitempty"`
	SlotCount   int      `json:"slotCount"`
	SignedCount int      `json:"signedCount"`
	YourTurn    bool     `json:"yourTurn"`
}

// listResponse is the keyset-paged listing of the caller's envelopes.
type listResponse struct {
	Envelopes  []summaryView `json:"envelopes"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// signingTaskView is one envelope awaiting the caller's signature in the signer inbox:
// the envelope summary, the caller's own slot, and whether it is the caller's turn under
// the order policy. The owner subject is never exposed (it is not the caller's to see).
type signingTaskView struct {
	Envelope   summaryView `json:"envelope"`
	SlotID     string      `json:"slotId"`
	OrderIndex int         `json:"orderIndex"`
	SlotStatus string      `json:"slotStatus"`
	SlotFlow   string      `json:"slotFlow,omitempty"`
	YourTurn   bool        `json:"yourTurn"`
}

// signingTasksResponse is the keyset-paged signer inbox.
type signingTasksResponse struct {
	Tasks      []signingTaskView `json:"tasks"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

// rfc3339 formats a timestamp as RFC3339 (UTC), or "" when it is the zero time.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.UTC().Format(time.RFC3339Nano)
}

// toEnvelopeView maps the stored read onto the API response shape.
func toEnvelopeView(v *store.EnvelopeView) *envelopeView {
	out := &envelopeView{
		Envelope: envelopeHeaderView{
			ID:          v.Envelope.ID,
			Owner:       v.Envelope.Owner,
			TenantID:    v.Envelope.TenantID,
			Title:       v.Envelope.Title,
			Status:      v.Envelope.Status,
			OrderPolicy: v.Envelope.OrderPolicy,
			Profile:     v.Envelope.Profile,
			Version:     v.Envelope.Version,
			CreatedAt:   rfc3339(v.Envelope.CreatedAt),
		},
		Slots:     make([]slotView, len(v.Slots)),
		Documents: make([]docRefView, len(v.Documents)),
	}
	for i, s := range v.Slots {
		out.Slots[i] = slotView{
			ID:           s.ID,
			OrderIndex:   s.OrderIndex,
			IdentityRef:  s.IdentityRef,
			Role:         s.Role,
			Flow:         s.Flow,
			RequiredLoA:  s.RequiredLoA,
			Status:       s.Status,
			JobID:        s.JobID,
			SignatureID:  s.SignatureID,
			SignedDocRef: s.SignedDocRef,
			SignedAt:     rfc3339(s.SignedAt),
			SignerName:   s.SignerName,
		}
	}
	for i, d := range v.Documents {
		out.Documents[i] = docRefView{DocumentID: d.DocumentID, ContentHash: d.ContentHash}
	}

	return out
}

// toSummaries maps stored envelope summaries onto the listing response shape, carrying
// the creation time and the progress projection (slot/signed counts + the owner's turn).
func toSummaries(envs []store.EnvelopeSummary) []summaryView {
	out := make([]summaryView, len(envs))
	for i, e := range envs {
		out[i] = summaryView{
			ID:          e.ID,
			Title:       e.Title,
			Status:      e.Status,
			OrderPolicy: e.OrderPolicy,
			Version:     e.Version,
			CreatedAt:   rfc3339(e.CreatedAt),
			UpdatedAt:   rfc3339(e.UpdatedAt),
			DocIDs:      e.DocIDs,
			SlotCount:   e.SlotCount,
			SignedCount: e.SignedCount,
			YourTurn:    e.YourTurn,
		}
	}

	return out
}

// toSigningTasks maps stored signer-inbox tasks onto the response shape, exposing the
// envelope summary (never the owner subject) and the caller's own slot.
func toSigningTasks(tasks []store.SigningTask) []signingTaskView {
	out := make([]signingTaskView, len(tasks))
	for i, tk := range tasks {
		out[i] = signingTaskView{
			Envelope: summaryView{
				ID:          tk.Envelope.ID,
				Title:       tk.Envelope.Title,
				Status:      tk.Envelope.Status,
				OrderPolicy: tk.Envelope.OrderPolicy,
				Version:     tk.Envelope.Version,
				DocIDs:      tk.Envelope.DocIDs,
			},
			SlotID:     tk.Slot.ID,
			OrderIndex: tk.Slot.OrderIndex,
			SlotStatus: tk.Slot.Status,
			SlotFlow:   tk.Slot.Flow,
			YourTurn:   tk.YourTurn,
		}
	}

	return out
}

// transitionResponse is the result of an applied state-machine transition.
type transitionResponse struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Version int    `json:"version"`
}

// slotIDResponse is the result of adding a slot.
type slotIDResponse struct {
	ID string `json:"id"`
}

// eligibleResponse is the order-policy precondition answer.
type eligibleResponse struct {
	Eligible bool `json:"eligible"`
}

// signedResponse is the result of the slot-signed callback. Idempotent reports
// true when a replayed callback was a no-op. EnvelopeStatus is the envelope's
// status after the callback (e.g. completed once the last slot signs), so the
// caller learns the roll-up without an owner-scoped read.
type signedResponse struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Idempotent     bool   `json:"idempotent,omitempty"`
	EnvelopeStatus string `json:"envelopeStatus,omitempty"`
}
