// Package store persists the envelope/workflow service's durable state — the
// envelope, its document references, and its signer slots — reached ONLY through
// SECURITY DEFINER procedures under an EXECUTE-only role; an in-memory backend
// exists for development/test. No backend exposes raw table access — every
// operation is a procedure call. The document bytes and the signature evidence
// live in their own services — never here; this store holds plain references only.
package store

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors the procedure error codes map onto, so callers/routes can pick
// the right HTTP status. The procedures return `<domain>:<reason>` codes:
// `:not_found` -> 404, `:conflict` -> 409, `:duplicate` -> 409, any other
// reason -> 422.
var (
	// ErrNotFound is returned when an envelope or slot is absent or not owned by
	// the caller (owner-filtered, so absent-or-not-owned is indistinguishable — no
	// resource-enumeration leak).
	ErrNotFound = errors.New("envelope: not found")
	// ErrConflict is returned when an optimistic-concurrency transition hits a
	// stale version (compare-and-set failed).
	ErrConflict = errors.New("envelope: version conflict")
	// ErrDuplicate is returned when a document is already attached or a slot order
	// index is already used.
	ErrDuplicate = errors.New("envelope: duplicate")
	// ErrInvalid is returned for a domain validation failure (a bad value or a
	// transition not permitted in the current state).
	ErrInvalid = errors.New("envelope: invalid")
	// ErrSlotLimit is returned when an envelope already holds as many signer slots
	// as this edition provides and another signer is added.
	ErrSlotLimit = errors.New("envelope: signer slot limit reached")
)

// SignerSlotsPerEnvelope is how many signer slots one envelope may hold.
//
// This is the product's shape rather than an operational limit: signing here runs
// between two parties, a person and one counterparty. Coordinating a document
// through more parties than that — routing, chasing, delegation, bulk send — is a
// separate product, and its code does not live in this repository. So the number is
// a named constant with its reason beside it rather than a configuration value: an
// operator tunes behaviour, and this is not behaviour to tune.
//
// Approvers and observers are not signers and are not counted.
const SignerSlotsPerEnvelope = 2

// signerSlotLimit is SignerSlotsPerEnvelope, indirected only so this package's own
// tests can exercise the boundary from both sides. Nothing reads it from the
// environment, and nothing outside this package can set it.
var signerSlotLimit = SignerSlotsPerEnvelope

// Envelope is one envelope row: the signing transaction and its state-machine
// fields. Version is the optimistic-concurrency token bumped on every transition.
type Envelope struct {
	ID          string    `json:"id"`
	Owner       string    `json:"owner"`
	TenantID    string    `json:"tenant_id,omitempty"`
	Title       string    `json:"title,omitempty"`
	Status      string    `json:"status"`
	OrderPolicy string    `json:"order_policy"`
	Profile     string    `json:"profile,omitempty"`
	Expiry      time.Time `json:"expiry,omitempty"`
	// RetentionUntil is when this envelope's documents stop being downloadable —
	// recorded at the terminal transition, and the clock the keep window is measured
	// from. Zero until then.
	RetentionUntil time.Time `json:"retention_until,omitempty"`
	Version        int       `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// EnvelopeSummary is one envelope in the owner's listing: the envelope row plus a
// progress projection. SlotCount/SignedCount give "n of N signed"; YourTurn is true
// when the envelope is actionable and the owner has an outstanding, order-eligible slot
// of their own (a slot with no bound counterparty identity) — i.e. it is the owner's turn
// to sign. These three are list-only projections computed by the query, not stored columns.
type EnvelopeSummary struct {
	Envelope
	// DocIDs are the envelope's attached document ids (document-store ids), so
	// a listing consumer can tell which documents an envelope covers without a
	// per-envelope detail fetch.
	DocIDs      []string `json:"doc_ids"`
	SlotCount   int      `json:"slot_count"`
	SignedCount int      `json:"signed_count"`
	YourTurn    bool     `json:"your_turn"`
}

// DocumentRef is one attached document reference: the document-store id and the
// content hash pinned at attach (the "same document" invariant). No bytes here.
type DocumentRef struct {
	EnvelopeID  string    `json:"envelope_id"`
	DocumentID  string    `json:"document_id"`
	ContentHash string    `json:"content_hash"`
	AddedAt     time.Time `json:"added_at"`
}

// Slot is one signer-slot row: a signer position in the envelope plus the linkage
// back to the signing job/signature it is fulfilled by.
type Slot struct {
	ID           string    `json:"id"`
	EnvelopeID   string    `json:"envelope_id"`
	OrderIndex   int       `json:"order_index"`
	IdentityRef  string    `json:"identity_ref,omitempty"`
	Role         string    `json:"role"`
	Flow         string    `json:"flow,omitempty"`
	RequiredLoA  string    `json:"required_loa,omitempty"`
	Status       string    `json:"status"`
	JobID        string    `json:"job_id,omitempty"`
	SignatureID  string    `json:"signature_id,omitempty"`
	SignedDocRef string    `json:"signed_doc_ref,omitempty"`
	SignedAt     time.Time `json:"signed_at,omitempty"`
	// SignerName is the display name of the person who fills this slot, captured from
	// their own authenticated session when they first participate (never from a typed
	// identity code). Empty until then.
	SignerName string `json:"signer_name,omitempty"`
}

// EnvelopeView is the nested read of one envelope: the envelope plus its slots and
// document references, owner-filtered.
type EnvelopeView struct {
	Envelope  Envelope      `json:"envelope"`
	Slots     []Slot        `json:"slots"`
	Documents []DocumentRef `json:"documents"`
}

// SigningTask is one envelope awaiting the caller's signature in the signer inbox: the
// envelope, the caller's own slot, and whether it is the caller's turn under the order
// policy (always so for a parallel envelope; for a sequential one, only once every
// lower-order slot is signed).
type SigningTask struct {
	Envelope TaskEnvelope `json:"envelope"`
	Slot     Slot         `json:"slot"`
	YourTurn bool         `json:"your_turn"`
}

// TaskEnvelope is the task's envelope: the envelope row plus its attached document
// ids (the same projection the owner's listing carries), so an inbox consumer that
// composes envelopes and standalone documents into one view can tell which documents
// an invited envelope covers — without a per-envelope detail fetch.
type TaskEnvelope struct {
	Envelope
	DocIDs []string `json:"doc_ids"`
}

// CreateEnvelopeInput is a new draft envelope. Owner is required; the rest are
// optional (the defaults of the data layer apply for empty values).
type CreateEnvelopeInput struct {
	Owner       string
	TenantID    string
	Title       string
	OrderPolicy string
	Profile     string
	Expiry      string
}

// AddSlotInput defines one signer slot on a draft envelope.
type AddSlotInput struct {
	EnvelopeID  string
	Owner       string
	OrderIndex  int
	IdentityRef string
	Role        string
	Flow        string
	RequiredLoA string
}

// Created is the result of creating an envelope: its assigned id, status, version.
type Created struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Version int    `json:"version"`
}

// Transition is the result of an applied state-machine transition.
type Transition struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Version int    `json:"version"`
}

// SignedResult is the result of the slot-signed callback. Idempotent reports true
// when a replayed callback was a no-op. EnvelopeStatus is the envelope's status
// after the callback (so the caller can act on a roll-up to completed without an
// owner-scoped read, which the signing-service caller cannot make).
type SignedResult struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	Idempotent     bool   `json:"idempotent,omitempty"`
	EnvelopeStatus string `json:"envelope_status,omitempty"`
	// DocIDs are the envelope's attached document ids, so the caller can
	// administer chain-level state (lift the download freeze) when the callback
	// rolled the envelope to a terminal status — without an owner-scoped read
	// it has no authority for.
	DocIDs []string `json:"doc_ids,omitempty"`
}

// DeclineResult is the result of a slot decline: the slot, plus the envelope's
// status after it (a decline drives the envelope terminal) and the attached
// document ids for the same chain-state administration as SignedResult.
type DeclineResult struct {
	ID             string   `json:"id"`
	Status         string   `json:"status"`
	EnvelopeStatus string   `json:"envelope_status,omitempty"`
	DocIDs         []string `json:"doc_ids,omitempty"`
}

// RetentionWindow names the instants one retention sweep judges against. Each
// stage is independent and a ZERO instant disables that stage, so a deployment can
// run only the parts it wants. The instants are computed by the caller from its own
// configuration — the data layer applies a window, it never invents one.
type RetentionWindow struct {
	// TerminalBefore: a finished envelope (completed, declined, cancelled, expired)
	// untouched since this instant is deleted, taking its signer slots and document
	// references with it.
	TerminalBefore time.Time
	// DraftBefore: a draft created before this instant is deleted as abandoned.
	DraftBefore time.Time
	// ExpireBefore: an envelope still open whose own expiry falls before this
	// instant becomes expired (the transition nothing else performs).
	ExpireBefore time.Time
	// Batch caps how many rows one pass may touch per stage, so a large backlog
	// drains over successive sweeps instead of one long lock. Zero takes the data
	// layer's default.
	Batch int
}

// SweepResult reports one retention pass. Expired carries the ids that moved to
// expired, so the caller can announce each transition.
type SweepResult struct {
	Expired         []string `json:"expired"`
	ExpiredCount    int      `json:"expired_count"`
	TerminalDeleted int      `json:"terminal_deleted"`
	DraftsDeleted   int      `json:"drafts_deleted"`
	// AwaitingHorizon is how many terminal envelopes the sweep could not judge
	// because their retention horizon was never recorded. It is the one way the
	// policy silently stops applying to a row, so it is reported rather than left
	// as an absence nobody notices.
	AwaitingHorizon int `json:"awaiting_horizon"`
}

// UnsettledEnvelope is a terminal envelope whose retention horizon was never
// recorded, together with the documents it is waiting on. Three different failures
// land a row here — the envelope was expired by the sweep itself (a transition with
// no request behind it), the read at the transition failed, or the row predates the
// horizon being recorded at all — and all three take the same repair, so they are
// one list.
type UnsettledEnvelope struct {
	ID        string   `json:"id"`
	Documents []string `json:"documents"`
}

// Total is how many rows the pass touched across all three stages — the number
// that says whether the backlog is drained.
func (r *SweepResult) Total() int {
	if r == nil {
		return 0
	}

	return r.ExpiredCount + r.TerminalDeleted + r.DraftsDeleted
}

// Store is the envelope service's persistence contract. It maps 1:1 onto the
// envelope schema procedures.
type Store interface {
	// CreateEnvelope creates a draft envelope (envelope.create_envelope) and
	// returns its assigned id, status, and version.
	CreateEnvelope(ctx context.Context, in CreateEnvelopeInput) (*Created, error)
	// GetEnvelope reads one envelope plus its slots and document references
	// (envelope.get_envelope). The owner may read it; a participant — a caller whose
	// authenticated eIDAS serial (callerSerial) matches a slot's identity_ref on a
	// non-draft envelope — may also read it. Returns ErrNotFound when absent or the
	// caller is neither (no enumeration). callerSerial "" means owner-only.
	GetEnvelope(ctx context.Context, id, owner, callerSerial string) (*EnvelopeView, error)
	// ListEnvelopes returns the caller's envelopes with their progress projection,
	// keyset-paged on the id (envelope.list_envelopes). cursor is the last id of the
	// prior page ("" for the first page); limit 0 takes the data layer's default.
	ListEnvelopes(ctx context.Context, owner, cursor string, limit int) ([]EnvelopeSummary, error)
	// FindEnvelopesForDocument returns the envelopes covering one document that the
	// caller may see (envelope.find_envelopes_for_document), newest first: the owner
	// always; a participant — callerSerial matching a slot's identity_ref — on a
	// non-draft envelope. Lets a document-centric view resolve "which envelope
	// carries this document?" without listing everything, including for a
	// participant revisiting a completed document. limit 0 takes the data layer's
	// default; callerSerial "" means owner-only.
	FindEnvelopesForDocument(ctx context.Context, owner, callerSerial, documentID string, limit int) ([]EnvelopeSummary, error)
	// ListSigningTasks returns the caller's signer inbox (envelope.list_signing_tasks):
	// non-draft envelopes where the caller's authenticated eIDAS serial (callerSerial)
	// matches a slot whose signature is still outstanding — "awaiting your signature".
	// Keyed on the serial, not ownership; envelopes the caller owns are excluded (owner is
	// the caller's subject, used only for that exclusion). callerSerial is required (empty
	// → ErrInvalid). Keyset-paged on the envelope id; cursor is the last envelope id of the
	// prior page ("" for the first); limit 0 takes the data layer's default.
	ListSigningTasks(ctx context.Context, callerSerial, owner, cursor string, limit int) ([]SigningTask, error)
	// AttachDocument pins a document reference onto a draft envelope, owner-filtered
	// (envelope.attach_document). The content hash is pinned by the caller after the
	// reference is validated on behalf of the user. Returns ErrDuplicate when the
	// document is already attached.
	AttachDocument(ctx context.Context, envelopeID, owner, documentID, contentHash string) error
	// AddSlot adds a signer slot to a draft envelope, owner-filtered
	// (envelope.add_slot), and returns the slot id. Returns ErrDuplicate when the
	// order index is already used.
	AddSlot(ctx context.Context, in AddSlotInput) (slotID string, err error)
	// ApplyTransition applies a state-machine transition with optimistic
	// compare-and-set on the version (envelope.apply_transition). Returns
	// ErrConflict on a stale version, ErrNotFound when absent or not owned.
	ApplyTransition(ctx context.Context, id, owner, toStatus string, expectedVersion int) (*Transition, error)
	// SlotEligible reports whether a slot may be signed under the envelope's order
	// policy (envelope.slot_eligible). The owner may check any slot; a participant
	// (callerSerial matches the slot's identity_ref on a non-draft envelope) may check
	// only their own. Returns ErrNotFound when the slot is absent or the caller is
	// neither. callerSerial "" means owner-only.
	SlotEligible(ctx context.Context, envelopeID, slotID, owner, callerSerial string) (eligible bool, err error)
	// SetSlotJob records the signing job id on a slot when signing starts and advances
	// the envelope on first signing (envelope.set_slot_job). The owner may set it on any
	// slot; a participant (callerSerial matches the slot's identity_ref on a non-draft
	// envelope) may set it only on their own. Returns ErrNotFound when the slot is
	// absent, the caller is neither, or it is not signable. callerSerial "" means owner-only.
	SetSlotJob(ctx context.Context, envelopeID, slotID, owner, callerSerial, jobID string) error
	// MarkSlotSigned records that a slot was finalized by the signing service and
	// advances the envelope, rolling up to completed once every slot is signed
	// (envelope.mark_slot_signed). NOT owner-scoped — the signing service, not the
	// owner, calls it. Idempotent: a replayed callback is a no-op success.
	MarkSlotSigned(ctx context.Context, envelopeID, slotID, signatureID, signedDocRef, jobID string) (*SignedResult, error)
	// DeclineSlot records that a signer declined a slot and drives the envelope to
	// declined (envelope.decline_slot). The owner may decline any slot; a participant
	// (callerSerial matches the slot's identity_ref on a non-draft envelope) may decline
	// only their own. Returns ErrNotFound when the slot is absent, the caller is neither,
	// or it is already signed. callerSerial "" means owner-only. The result carries the
	// envelope's post-decline status + doc ids (a decline drives the envelope terminal).
	DeclineSlot(ctx context.Context, envelopeID, slotID, owner, callerSerial string) (*DeclineResult, error)
	// CaptureSignerName records a participant's display name on their own slot
	// (envelope.capture_signer_name), supplied from that person's authenticated session.
	// The owner may name their own (identity-less) slot; a participant may name only their
	// own (callerSerial match on a non-draft envelope). Write-once: a no-op if already set
	// or the caller is not entitled to the slot (idempotent success — no enumeration).
	CaptureSignerName(ctx context.Context, envelopeID, slotID, owner, callerSerial, name string) error
	// SetRetentionUntil records when this envelope's documents stop being
	// downloadable (envelope.set_retention_until) — the clock the retention sweep
	// measures the keep window from. Written at the terminal transition with the
	// value read from the document service; it cannot be computed here, because
	// document retention rolls forward on every signing act. NOT owner-scoped: the
	// transition can arrive on the signing service's callback, with no owner in the
	// request. Returns ErrNotFound when the envelope is absent.
	SetRetentionUntil(ctx context.Context, id string, until time.Time) error
	// SweepRetention runs one retention pass (envelope.sweep_retention): open
	// envelopes past their expiry become expired, finished envelopes past the keep
	// window are deleted with their slots and document references, and abandoned
	// drafts are deleted. NOT owner-scoped — this is the service's own background
	// housekeeping, not a user action. Returns ErrInvalid when the window names no
	// instant at all (a sweep with no window is a misconfiguration, not a no-op).
	SweepRetention(ctx context.Context, window RetentionWindow) (*SweepResult, error)
	// ListUnsettledTerminal returns terminal envelopes whose retention horizon was
	// never recorded (envelope.list_unsettled_terminal), with the documents each is
	// waiting on — the rows SweepRetention refuses to judge, so that something can
	// go and settle them. Without it AwaitingHorizon can only ever grow. Unordered:
	// the caller drains the set over successive passes. NOT owner-scoped — service
	// housekeeping. Returns ErrInvalid when limit is negative.
	ListUnsettledTerminal(ctx context.Context, limit int) ([]UnsettledEnvelope, error)
	// Ping verifies backend connectivity for readiness checks.
	Ping(ctx context.Context) error
	// Close releases backend resources.
	Close()
}
