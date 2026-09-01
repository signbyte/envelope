package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the platform store: the envelope schema reached ONLY through
// SECURITY DEFINER procedures under an EXECUTE-only role. This package never issues
// raw table SQL — it only CALLs the procedures.
//
// Selected when the store DSN is set; the in-memory backend is the dev/test
// default.
type Postgres struct {
	pool *pgxpool.Pool
}

// NewPostgres opens a connection pool to the platform PostgreSQL. The pool is
// lazy; connectivity is verified on first use (or via Ping).
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: postgres connect: %w", err)
	}

	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (p *Postgres) Close() { p.pool.Close() }

// Ping verifies backend connectivity.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// envelope is the structured result every procedure returns
// (util.result_success / util.result_error).
type envelope struct {
	Result  string          `json:"result"`
	Data    json.RawMessage `json:"data"`
	Code    string          `json:"code"`
	Message string          `json:"message"`
}

// mapCode turns a <domain>:<reason> error code into a sentinel error where one
// exists, so callers/routes can pick the right HTTP status. An unrecognized reason
// falls through to ErrInvalid (a domain validation failure -> 422).
func mapCode(proc, code, msg string) error {
	switch {
	case strings.HasSuffix(code, ":not_found"):
		return ErrNotFound
	case strings.HasSuffix(code, ":conflict"):
		return ErrConflict
	case strings.HasSuffix(code, ":duplicate"):
		return ErrDuplicate
	case strings.HasSuffix(code, ":slot_limit"):
		return ErrSlotLimit
	case strings.HasSuffix(code, ":invalid"):
		return fmt.Errorf("%w: %s", ErrInvalid, msg)
	default:
		return fmt.Errorf("store: %s: %s: %s", proc, code, msg)
	}
}

// call invokes a SECURITY DEFINER procedure with the uniform JSONB envelope and
// returns the decoded `data` payload, or a typed error from result_error.
func (p *Postgres) call(ctx context.Context, proc string, in any) (json.RawMessage, error) {
	inJSON, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("store: marshal input: %w", err)
	}

	// CALL with an INOUT parameter returns a single-column row carrying po_data;
	// NULL seeds the INOUT slot.
	q := fmt.Sprintf("CALL %s($1::jsonb, NULL::jsonb)", proc)

	var out []byte
	if err := p.pool.QueryRow(ctx, q, inJSON).Scan(&out); err != nil {
		// A procedure that fails after a write re-raises a structured error with
		// SQLSTATE P0001; its message is the result_error JSON.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "P0001" {
			var env envelope
			if json.Unmarshal([]byte(pgErr.Message), &env) == nil && env.Result == "error" {
				return nil, mapCode(proc, env.Code, env.Message)
			}
		}

		return nil, fmt.Errorf("store: %s: %w", proc, err)
	}

	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("store: %s: decode result: %w", proc, err)
	}
	if env.Result != "success" {
		return nil, mapCode(proc, env.Code, env.Message)
	}

	return env.Data, nil
}

// CreateEnvelope creates a draft envelope via envelope.create_envelope.
func (p *Postgres) CreateEnvelope(ctx context.Context, in CreateEnvelopeInput) (*Created, error) {
	body := map[string]any{"owner": in.Owner}
	putOpt(body, "tenant_id", in.TenantID)
	putOpt(body, "title", in.Title)
	putOpt(body, "order_policy", in.OrderPolicy)
	putOpt(body, "profile", in.Profile)
	putOpt(body, "expiry", in.Expiry)

	data, err := p.call(ctx, "envelope.create_envelope", body)
	if err != nil {
		return nil, err
	}

	var out Created
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: create_envelope: decode: %w", err)
	}

	return &out, nil
}

// GetEnvelope reads one envelope plus slots and documents via envelope.get_envelope.
func (p *Postgres) GetEnvelope(ctx context.Context, id, owner, callerSerial string) (*EnvelopeView, error) {
	body := map[string]any{"id": id, "owner": owner}
	putOpt(body, "caller_serial", callerSerial)

	data, err := p.call(ctx, "envelope.get_envelope", body)
	if err != nil {
		return nil, err
	}

	var out EnvelopeView
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: get_envelope: decode: %w", err)
	}

	return &out, nil
}

// ListEnvelopes returns the caller's envelopes via envelope.list_envelopes.
func (p *Postgres) ListEnvelopes(ctx context.Context, owner, cursor string, limit int) ([]EnvelopeSummary, error) {
	body := map[string]any{"owner": owner}
	putOpt(body, "cursor", cursor)
	if limit > 0 {
		body["limit"] = limit
	}

	data, err := p.call(ctx, "envelope.list_envelopes", body)
	if err != nil {
		return nil, err
	}

	var out struct {
		Envelopes []EnvelopeSummary `json:"envelopes"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: list_envelopes: decode: %w", err)
	}

	return out.Envelopes, nil
}

// FindEnvelopesForDocument returns the envelopes covering one document that the
// caller may see, via envelope.find_envelopes_for_document.
func (p *Postgres) FindEnvelopesForDocument(ctx context.Context, owner, callerSerial, documentID string, limit int) ([]EnvelopeSummary, error) {
	body := map[string]any{"owner": owner, "document_id": documentID}
	putOpt(body, "caller_serial", callerSerial)
	if limit > 0 {
		body["limit"] = limit
	}

	data, err := p.call(ctx, "envelope.find_envelopes_for_document", body)
	if err != nil {
		return nil, err
	}

	var out struct {
		Envelopes []EnvelopeSummary `json:"envelopes"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: find_envelopes_for_document: decode: %w", err)
	}

	return out.Envelopes, nil
}

// ListSigningTasks returns the caller's signer inbox via envelope.list_signing_tasks.
func (p *Postgres) ListSigningTasks(ctx context.Context, callerSerial, owner, cursor string, limit int) ([]SigningTask, error) {
	body := map[string]any{"caller_serial": callerSerial}
	putOpt(body, "owner", owner)
	putOpt(body, "cursor", cursor)
	if limit > 0 {
		body["limit"] = limit
	}

	data, err := p.call(ctx, "envelope.list_signing_tasks", body)
	if err != nil {
		return nil, err
	}

	var out struct {
		Tasks []SigningTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: list_signing_tasks: decode: %w", err)
	}

	return out.Tasks, nil
}

// AttachDocument pins a document reference via envelope.attach_document.
func (p *Postgres) AttachDocument(ctx context.Context, envelopeID, owner, documentID, contentHash string) error {
	_, err := p.call(ctx, "envelope.attach_document", map[string]any{
		"envelope_id":  envelopeID,
		"owner":        owner,
		"document_id":  documentID,
		"content_hash": contentHash,
	})

	return err
}

// AddSlot adds a signer slot via envelope.add_slot.
func (p *Postgres) AddSlot(ctx context.Context, in AddSlotInput) (string, error) {
	body := map[string]any{
		"envelope_id": in.EnvelopeID,
		"owner":       in.Owner,
		"order_index": in.OrderIndex,
	}
	putOpt(body, "identity_ref", in.IdentityRef)
	putOpt(body, "role", in.Role)
	putOpt(body, "flow", in.Flow)
	putOpt(body, "required_loa", in.RequiredLoA)
	// The signer count is enforced inside the procedure rather than here: two
	// concurrent adds would each read the same count and each insert. The limit
	// travels with the call so the constant above stays its single home.
	body["max_signer_slots"] = signerSlotLimit

	return p.callID(ctx, "envelope.add_slot", body)
}

// ApplyTransition applies a compare-and-set transition via envelope.apply_transition.
func (p *Postgres) ApplyTransition(ctx context.Context, id, owner, toStatus string, expectedVersion int) (*Transition, error) {
	data, err := p.call(ctx, "envelope.apply_transition", map[string]any{
		"id":               id,
		"owner":            owner,
		"to_status":        toStatus,
		"expected_version": expectedVersion,
	})
	if err != nil {
		return nil, err
	}

	var out Transition
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: apply_transition: decode: %w", err)
	}

	return &out, nil
}

// SlotEligible reports order-policy eligibility via envelope.slot_eligible.
func (p *Postgres) SlotEligible(ctx context.Context, envelopeID, slotID, owner, callerSerial string) (bool, error) {
	body := map[string]any{
		"envelope_id": envelopeID,
		"slot_id":     slotID,
		"owner":       owner,
	}
	putOpt(body, "caller_serial", callerSerial)

	data, err := p.call(ctx, "envelope.slot_eligible", body)
	if err != nil {
		return false, err
	}

	var out struct {
		Eligible bool `json:"eligible"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return false, fmt.Errorf("store: slot_eligible: decode: %w", err)
	}

	return out.Eligible, nil
}

// SetSlotJob records the job id on a slot via envelope.set_slot_job.
func (p *Postgres) SetSlotJob(ctx context.Context, envelopeID, slotID, owner, callerSerial, jobID string) error {
	body := map[string]any{
		"envelope_id": envelopeID,
		"slot_id":     slotID,
		"owner":       owner,
		"job_id":      jobID,
	}
	putOpt(body, "caller_serial", callerSerial)

	_, err := p.call(ctx, "envelope.set_slot_job", body)

	return err
}

// MarkSlotSigned records the slot-signed callback via envelope.mark_slot_signed.
func (p *Postgres) MarkSlotSigned(ctx context.Context, envelopeID, slotID, signatureID, signedDocRef, jobID string) (*SignedResult, error) {
	body := map[string]any{
		"envelope_id":  envelopeID,
		"slot_id":      slotID,
		"signature_id": signatureID,
	}
	putOpt(body, "signed_doc_ref", signedDocRef)
	putOpt(body, "job_id", jobID)

	data, err := p.call(ctx, "envelope.mark_slot_signed", body)
	if err != nil {
		return nil, err
	}

	var out SignedResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: mark_slot_signed: decode: %w", err)
	}

	return &out, nil
}

// DeclineSlot records a decline via envelope.decline_slot.
func (p *Postgres) DeclineSlot(ctx context.Context, envelopeID, slotID, owner, callerSerial string) (*DeclineResult, error) {
	body := map[string]any{
		"envelope_id": envelopeID,
		"slot_id":     slotID,
		"owner":       owner,
	}
	putOpt(body, "caller_serial", callerSerial)

	data, err := p.call(ctx, "envelope.decline_slot", body)
	if err != nil {
		return nil, err
	}

	var out DeclineResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: decline_slot: decode: %w", err)
	}

	return &out, nil
}

// CaptureSignerName records a participant's display name on their own slot via
// envelope.capture_signer_name (write-once; an unentitled/already-named slot is an
// idempotent no-op success).
func (p *Postgres) CaptureSignerName(ctx context.Context, envelopeID, slotID, owner, callerSerial, name string) error {
	body := map[string]any{
		"envelope_id": envelopeID,
		"slot_id":     slotID,
		"owner":       owner,
		"name":        name,
	}
	putOpt(body, "caller_serial", callerSerial)

	_, err := p.call(ctx, "envelope.capture_signer_name", body)

	return err
}

// SetRetentionUntil records the envelope's retention horizon via
// envelope.set_retention_until.
func (p *Postgres) SetRetentionUntil(ctx context.Context, id string, until time.Time) error {
	_, err := p.call(ctx, "envelope.set_retention_until", map[string]any{
		"id":              id,
		"retention_until": until.UTC().Format(time.RFC3339Nano),
	})

	return err
}

// SweepRetention runs one retention pass via envelope.sweep_retention. Only the
// instants the window actually names are sent, so an unset one leaves its stage out
// of the call rather than reaching the data layer as a zero time.
func (p *Postgres) SweepRetention(ctx context.Context, w RetentionWindow) (*SweepResult, error) {
	body := map[string]any{}
	putInstant(body, "terminal_before", w.TerminalBefore)
	putInstant(body, "draft_before", w.DraftBefore)
	putInstant(body, "expire_before", w.ExpireBefore)
	if w.Batch > 0 {
		body["batch"] = w.Batch
	}

	data, err := p.call(ctx, "envelope.sweep_retention", body)
	if err != nil {
		return nil, err
	}

	var out SweepResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: sweep_retention: decode: %w", err)
	}

	return &out, nil
}

// ListUnsettledTerminal reads the repair list via envelope.list_unsettled_terminal.
func (p *Postgres) ListUnsettledTerminal(ctx context.Context, limit int) ([]UnsettledEnvelope, error) {
	body := map[string]any{}
	if limit > 0 {
		body["batch"] = limit
	}

	data, err := p.call(ctx, "envelope.list_unsettled_terminal", body)
	if err != nil {
		return nil, err
	}

	var out struct {
		Envelopes []UnsettledEnvelope `json:"envelopes"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("store: list_unsettled_terminal: decode: %w", err)
	}

	return out.Envelopes, nil
}

// callID invokes a procedure that returns { id } and decodes it.
func (p *Postgres) callID(ctx context.Context, proc string, body map[string]any) (string, error) {
	data, err := p.call(ctx, proc, body)
	if err != nil {
		return "", err
	}

	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return "", fmt.Errorf("store: %s: decode: %w", proc, err)
	}

	return res.ID, nil
}

// putOpt adds key=val to body only when val is non-empty (so the procedure's
// COALESCE/NULLIF defaults apply for omitted optionals).
func putOpt(body map[string]any, key, val string) {
	if val != "" {
		body[key] = val
	}
}

// putInstant adds key=val to body only when the instant is set, so an unused
// retention stage is simply absent from the call rather than sent as the zero time
// (which would read as "the year 1" and match nothing, silently).
func putInstant(body map[string]any, key string, val time.Time) {
	if !val.IsZero() {
		body[key] = val.UTC().Format(time.RFC3339Nano)
	}
}
