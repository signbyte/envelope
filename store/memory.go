package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// defaultSweepBatch mirrors the data layer's own default for a retention pass that
// names no batch, so the two backends drain a backlog at the same rate.
const defaultSweepBatch = 500

// Memory is an in-memory Store for development/test (no DB). It is NOT durable —
// state is lost on restart — and exists so the service boots and the routes are
// testable without Postgres. It reproduces the data layer's owner-filtering,
// optimistic-concurrency compare-and-set, order-policy eligibility, idempotent
// slot-signed callback, and completion roll-up so behavior matches production.
type Memory struct {
	mu        sync.Mutex
	envelopes map[string]*Envelope
	slots     map[string][]*Slot        // envelope id -> slots (in insert order)
	docs      map[string][]*DocumentRef // envelope id -> document refs
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		envelopes: make(map[string]*Envelope),
		slots:     make(map[string][]*Slot),
		docs:      make(map[string][]*DocumentRef),
	}
}

// CreateEnvelope creates a draft envelope and returns its assigned id.
func (m *Memory) CreateEnvelope(_ context.Context, in CreateEnvelopeInput) (*Created, error) {
	if in.Owner == "" {
		return nil, ErrInvalid
	}
	policy := in.OrderPolicy
	if policy == "" {
		policy = "parallel"
	}
	if policy != "parallel" && policy != "sequential" {
		return nil, ErrInvalid
	}

	// An unparsable expiry is a bad request, exactly as it is for the data layer,
	// which would refuse the timestamp cast.
	var expiry time.Time
	if in.Expiry != "" {
		t, err := time.Parse(time.RFC3339, in.Expiry)
		if err != nil {
			return nil, ErrInvalid
		}
		expiry = t.UTC()
	}

	id := ulid.Make().String()
	now := time.Now().UTC()
	m.mu.Lock()
	m.envelopes[id] = &Envelope{
		ID: id, Owner: in.Owner, TenantID: in.TenantID, Title: in.Title,
		Status: "draft", OrderPolicy: policy, Profile: in.Profile, Expiry: expiry,
		Version: 0, CreatedAt: now, UpdatedAt: now,
	}
	m.mu.Unlock()

	return &Created{ID: id, Status: "draft", Version: 0}, nil
}

// GetEnvelope reads one envelope plus its slots and documents. The owner may read it;
// a participant (callerSerial matches a slot's identity_ref on a non-draft envelope)
// may also read it.
func (m *Memory) GetEnvelope(_ context.Context, id, owner, callerSerial string) (*EnvelopeView, error) {
	if id == "" || owner == "" {
		return nil, ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[id]
	if !ok || !m.canRead(e, owner, callerSerial) {
		return nil, ErrNotFound
	}

	view := &EnvelopeView{Envelope: *e, Slots: []Slot{}, Documents: []DocumentRef{}}
	for _, s := range m.sortedSlots(id) {
		view.Slots = append(view.Slots, *s)
	}
	for _, d := range m.docs[id] {
		view.Documents = append(view.Documents, *d)
	}

	return view, nil
}

// ListEnvelopes returns the caller's envelopes with their progress projection,
// keyset-paged on the id (DESC).
func (m *Memory) ListEnvelopes(_ context.Context, owner, cursor string, limit int) ([]EnvelopeSummary, error) {
	if owner == "" {
		return nil, ErrInvalid
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var ids []string
	for id, e := range m.envelopes {
		if e.Owner == owner && (cursor == "" || id < cursor) {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	out := []EnvelopeSummary{}
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		e := m.envelopes[id]
		docIDs := make([]string, 0, len(m.docs[id]))
		for _, d := range m.docs[id] {
			docIDs = append(docIDs, d.DocumentID)
		}
		sort.Strings(docIDs)
		out = append(out, EnvelopeSummary{
			Envelope:    *e,
			DocIDs:      docIDs,
			SlotCount:   len(m.slots[id]),
			SignedCount: m.countSigned(id),
			YourTurn:    m.ownerTurn(e),
		})
	}

	return out, nil
}

// FindEnvelopesForDocument returns the envelopes covering one document that the
// caller may see (the owner always; a serial-matched participant on a non-draft
// envelope), newest first.
func (m *Memory) FindEnvelopesForDocument(_ context.Context, owner, callerSerial, documentID string, limit int) ([]EnvelopeSummary, error) {
	if owner == "" || documentID == "" {
		return nil, ErrInvalid
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var ids []string
	for id, e := range m.envelopes {
		covers := false
		for _, d := range m.docs[id] {
			if d.DocumentID == documentID {
				covers = true

				break
			}
		}
		if covers && m.canRead(e, owner, callerSerial) {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	out := []EnvelopeSummary{}
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		e := m.envelopes[id]
		docIDs := make([]string, 0, len(m.docs[id]))
		for _, d := range m.docs[id] {
			docIDs = append(docIDs, d.DocumentID)
		}
		sort.Strings(docIDs)
		out = append(out, EnvelopeSummary{
			Envelope:    *e,
			DocIDs:      docIDs,
			SlotCount:   len(m.slots[id]),
			SignedCount: m.countSigned(id),
			YourTurn:    m.callerTurn(e, owner, callerSerial),
		})
	}

	return out, nil
}

// callerTurn reports whether it is the CALLER's turn on the envelope: the owner's
// outstanding own slot (no bound counterparty identity), or the serial-matched
// participant's outstanding slot — each only when order-eligible. Caller holds the lock.
func (m *Memory) callerTurn(e *Envelope, owner, callerSerial string) bool {
	if e.Status != "sent" && e.Status != "in_progress" {
		return false
	}
	for _, s := range m.slots[e.ID] {
		mine := (e.Owner == owner && s.IdentityRef == "") ||
			(callerSerial != "" && s.IdentityRef == callerSerial)
		if !mine {
			continue
		}
		if s.Status != "draft" && s.Status != "sent" && s.Status != "in_progress" {
			continue
		}
		if m.slotTurn(e, s) {
			return true
		}
	}

	return false
}

// countSigned counts the envelope's slots already signed. Caller holds the lock.
func (m *Memory) countSigned(envelopeID string) int {
	n := 0
	for _, s := range m.slots[envelopeID] {
		if s.Status == "signed" {
			n++
		}
	}

	return n
}

// ownerTurn reports whether the envelope is actionable and the owner has an outstanding,
// order-eligible slot of their own (one with no bound counterparty identity) — i.e. it is
// the owner's turn to sign, the signal the dashboard badge needs. Caller holds the lock.
func (m *Memory) ownerTurn(e *Envelope) bool {
	if e.Status != "sent" && e.Status != "in_progress" {
		return false
	}
	for _, s := range m.slots[e.ID] {
		if s.IdentityRef != "" {
			continue
		}
		if s.Status != "draft" && s.Status != "sent" && s.Status != "in_progress" {
			continue
		}
		if m.slotTurn(e, s) {
			return true
		}
	}

	return false
}

// ListSigningTasks returns the caller's signer inbox: non-draft envelopes where the caller's
// serial matches a slot whose signature is still outstanding (draft/sent/in_progress),
// excluding envelopes the caller owns. Newest envelope first, then by slot order.
func (m *Memory) ListSigningTasks(_ context.Context, callerSerial, owner, cursor string, limit int) ([]SigningTask, error) {
	if callerSerial == "" {
		return nil, ErrInvalid
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var ids []string
	for id, e := range m.envelopes {
		if owner != "" && e.Owner == owner {
			continue // the caller's own envelopes list elsewhere, not in the signer inbox
		}
		if e.Status != "sent" && e.Status != "in_progress" {
			continue
		}
		if cursor != "" && id >= cursor {
			continue
		}
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	out := []SigningTask{}
	for _, id := range ids {
		if len(out) >= limit {
			break
		}
		e := m.envelopes[id]
		for _, s := range m.sortedSlots(id) {
			if s.IdentityRef != callerSerial {
				continue
			}
			if s.Status != "draft" && s.Status != "sent" && s.Status != "in_progress" {
				continue
			}
			docIDs := make([]string, 0, len(m.docs[id]))
			for _, d := range m.docs[id] {
				docIDs = append(docIDs, d.DocumentID)
			}
			sort.Strings(docIDs)
			out = append(out, SigningTask{Envelope: TaskEnvelope{Envelope: *e, DocIDs: docIDs}, Slot: *s, YourTurn: m.slotTurn(e, s)})
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}

	return out, nil
}

// AttachDocument pins a document reference onto a draft envelope, owner-filtered.
func (m *Memory) AttachDocument(_ context.Context, envelopeID, owner, documentID, contentHash string) error {
	if envelopeID == "" || owner == "" || documentID == "" || contentHash == "" {
		return ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[envelopeID]
	if !ok || e.Owner != owner {
		return ErrNotFound
	}
	if e.Status != "draft" {
		return ErrInvalid
	}
	for _, d := range m.docs[envelopeID] {
		if d.DocumentID == documentID {
			return ErrDuplicate
		}
	}

	m.docs[envelopeID] = append(m.docs[envelopeID], &DocumentRef{
		EnvelopeID: envelopeID, DocumentID: documentID,
		ContentHash: contentHash, AddedAt: time.Now().UTC(),
	})

	return nil
}

// AddSlot adds a signer slot to a draft envelope, owner-filtered.
func (m *Memory) AddSlot(_ context.Context, in AddSlotInput) (string, error) {
	if in.EnvelopeID == "" || in.Owner == "" {
		return "", ErrInvalid
	}
	role := in.Role
	if role == "" {
		role = "signer"
	}
	if role != "signer" && role != "approver" && role != "observer" {
		return "", ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[in.EnvelopeID]
	if !ok || e.Owner != in.Owner {
		return "", ErrNotFound
	}
	if e.Status != "draft" {
		return "", ErrInvalid
	}
	signers := 0
	for _, s := range m.slots[in.EnvelopeID] {
		if s.OrderIndex == in.OrderIndex {
			return "", ErrDuplicate
		}
		if s.Role == "signer" {
			signers++
		}
	}
	// The edition's signer count. Counted, never derived from the order index:
	// callers number their slots from 0 or from 1 as they please, so an index bound
	// would refuse work that is inside the limit.
	if role == "signer" && signers >= signerSlotLimit {
		return "", ErrSlotLimit
	}

	id := ulid.Make().String()
	m.slots[in.EnvelopeID] = append(m.slots[in.EnvelopeID], &Slot{
		ID: id, EnvelopeID: in.EnvelopeID, OrderIndex: in.OrderIndex,
		IdentityRef: in.IdentityRef, Role: role, Flow: in.Flow,
		RequiredLoA: in.RequiredLoA, Status: "draft",
	})

	return id, nil
}

// ApplyTransition applies a compare-and-set transition on the version.
func (m *Memory) ApplyTransition(_ context.Context, id, owner, toStatus string, expectedVersion int) (*Transition, error) {
	if id == "" || owner == "" {
		return nil, ErrInvalid
	}
	if !validStatus(toStatus) {
		return nil, ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[id]
	if !ok || e.Owner != owner {
		return nil, ErrNotFound
	}
	if e.Version != expectedVersion {
		return nil, ErrConflict
	}

	e.Status = toStatus
	e.Version++
	e.UpdatedAt = time.Now().UTC()

	return &Transition{ID: id, Status: e.Status, Version: e.Version}, nil
}

// SlotEligible reports whether a slot may be signed under the order policy. The owner
// may check any slot; a participant may check only their own (callerSerial match).
func (m *Memory) SlotEligible(_ context.Context, envelopeID, slotID, owner, callerSerial string) (bool, error) {
	if envelopeID == "" || slotID == "" || owner == "" {
		return false, ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[envelopeID]
	if !ok {
		return false, ErrNotFound
	}
	slot := m.findSlot(envelopeID, slotID)
	if slot == nil || !canActOnSlot(e, slot, owner, callerSerial) {
		return false, ErrNotFound
	}

	envOpen := e.Status == "sent" || e.Status == "in_progress"
	slotOpen := slot.Status == "draft" || slot.Status == "sent" || slot.Status == "in_progress"
	if !envOpen || !slotOpen {
		return false, nil
	}
	if e.OrderPolicy == "parallel" {
		return true, nil
	}
	// sequential: eligible only when every lower order_index slot is signed.
	for _, s := range m.slots[envelopeID] {
		if s.OrderIndex < slot.OrderIndex && s.Status != "signed" {
			return false, nil
		}
	}

	return true, nil
}

// SetSlotJob records the job id on a slot and advances the envelope on first
// signing. The owner may set it on any slot; a participant may set it only on their
// own (callerSerial match).
func (m *Memory) SetSlotJob(_ context.Context, envelopeID, slotID, owner, callerSerial, jobID string) error {
	if envelopeID == "" || slotID == "" || owner == "" || jobID == "" {
		return ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[envelopeID]
	if !ok {
		return ErrNotFound
	}
	slot := m.findSlot(envelopeID, slotID)
	if slot == nil || !canActOnSlot(e, slot, owner, callerSerial) {
		return ErrNotFound
	}
	if slot.Status != "draft" && slot.Status != "sent" && slot.Status != "in_progress" {
		return ErrNotFound
	}

	slot.JobID = jobID
	slot.Status = "in_progress"
	if e.Status == "sent" {
		e.Status = "in_progress"
		e.Version++
		e.UpdatedAt = time.Now().UTC()
	}

	return nil
}

// MarkSlotSigned records the slot-signed callback (NOT owner-scoped) and advances
// the envelope, rolling up to completed once every slot is signed. Idempotent.
func (m *Memory) MarkSlotSigned(_ context.Context, envelopeID, slotID, signatureID, signedDocRef, jobID string) (*SignedResult, error) {
	if envelopeID == "" || slotID == "" {
		return nil, ErrInvalid
	}
	if signatureID == "" {
		return nil, ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[envelopeID]
	if !ok {
		return nil, ErrNotFound
	}
	slot := m.findSlot(envelopeID, slotID)
	if slot == nil {
		return nil, ErrNotFound
	}
	if slot.Status == "signed" {
		return &SignedResult{ID: slotID, Status: "signed", Idempotent: true, EnvelopeStatus: e.Status, DocIDs: m.docIDs(envelopeID)}, nil
	}

	slot.Status = "signed"
	slot.SignatureID = signatureID
	slot.SignedDocRef = signedDocRef
	if jobID != "" {
		slot.JobID = jobID
	}
	slot.SignedAt = time.Now().UTC()

	if e.Status == "sent" {
		e.Status = "in_progress"
		e.Version++
	}
	// Roll up to completed only when every slot is signed AND all signatures landed in
	// one shared container. Divergent containers (more than one distinct ref) must not
	// read as completed — that should be impossible (one container per chain), so leave
	// the envelope in_progress for reconciliation instead.
	if m.allSigned(envelopeID) && m.converged(envelopeID) && e.Status != "completed" {
		e.Status = "completed"
		e.Version++
	}
	e.UpdatedAt = time.Now().UTC()

	return &SignedResult{ID: slotID, Status: "signed", EnvelopeStatus: e.Status, DocIDs: m.docIDs(envelopeID)}, nil
}

// docIDs collects the envelope's attached document ids, sorted. Caller holds the lock.
func (m *Memory) docIDs(envelopeID string) []string {
	out := make([]string, 0, len(m.docs[envelopeID]))
	for _, d := range m.docs[envelopeID] {
		out = append(out, d.DocumentID)
	}
	sort.Strings(out)

	return out
}

// DeclineSlot records a decline and drives the envelope to declined. The owner may
// decline any slot; a participant may decline only their own (callerSerial match).
func (m *Memory) DeclineSlot(_ context.Context, envelopeID, slotID, owner, callerSerial string) (*DeclineResult, error) {
	if envelopeID == "" || slotID == "" || owner == "" {
		return nil, ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[envelopeID]
	if !ok {
		return nil, ErrNotFound
	}
	slot := m.findSlot(envelopeID, slotID)
	if slot == nil || !canActOnSlot(e, slot, owner, callerSerial) || slot.Status == "signed" {
		return nil, ErrNotFound
	}

	slot.Status = "declined"
	switch e.Status {
	case "declined", "cancelled", "completed", "expired":
		// envelope already terminal — leave it
	default:
		e.Status = "declined"
		e.Version++
		e.UpdatedAt = time.Now().UTC()
	}

	return &DeclineResult{ID: slotID, Status: "declined", EnvelopeStatus: e.Status, DocIDs: m.docIDs(envelopeID)}, nil
}

// CaptureSignerName records a participant's display name on their own slot, write-once.
// The owner may name their own slot; a participant may name only their own (callerSerial
// match on a non-draft envelope). A no-op — never an error — when the name is empty, the
// slot is absent, the caller is not entitled, or the slot is already named (idempotent).
func (m *Memory) CaptureSignerName(_ context.Context, envelopeID, slotID, owner, callerSerial, name string) error {
	if name == "" {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[envelopeID]
	if !ok {
		return nil
	}
	slot := m.findSlot(envelopeID, slotID)
	if slot == nil || !canActOnSlot(e, slot, owner, callerSerial) {
		return nil
	}
	if slot.SignerName == "" {
		slot.SignerName = name
	}

	return nil
}

// SetRetentionUntil records the envelope's retention horizon. Not owner-scoped and
// it does not move the version: this is bookkeeping about the envelope, not a
// transition of it (mirrors envelope.set_retention_until).
func (m *Memory) SetRetentionUntil(_ context.Context, id string, until time.Time) error {
	if id == "" {
		return ErrInvalid
	}
	if until.IsZero() {
		return ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[id]
	if !ok {
		return ErrNotFound
	}
	e.RetentionUntil = until.UTC()

	return nil
}

// SweepRetention runs one retention pass, reproducing the data layer's three stages
// and their order so the background task behaves the same with or without a
// database: expire first, then delete the finished, then the abandoned drafts.
// Because expiring touches the row, an envelope expired by this pass always starts
// its keep window here and is never deleted by the same pass.
func (m *Memory) SweepRetention(_ context.Context, w RetentionWindow) (*SweepResult, error) {
	if w.TerminalBefore.IsZero() && w.DraftBefore.IsZero() && w.ExpireBefore.IsZero() {
		return nil, ErrInvalid
	}
	batch := w.Batch
	if batch <= 0 {
		batch = defaultSweepBatch
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	out := &SweepResult{Expired: []string{}}
	now := time.Now().UTC()

	if !w.ExpireBefore.IsZero() {
		for _, e := range m.sweepCandidates(func(e *Envelope) bool {
			return (e.Status == "sent" || e.Status == "in_progress") &&
				!e.Expiry.IsZero() && e.Expiry.Before(w.ExpireBefore)
		}, batch) {
			e.Status = "expired"
			// The version moves so an actor still holding the pre-expiry version
			// loses its compare-and-set instead of resurrecting the envelope.
			e.Version++
			e.UpdatedAt = now
			out.Expired = append(out.Expired, e.ID)
		}
		out.ExpiredCount = len(out.Expired)
	}

	if !w.TerminalBefore.IsZero() {
		for _, e := range m.sweepCandidates(func(e *Envelope) bool {
			// The clock is the documents' horizon, not the row's own timestamp: the
			// envelope is the tracking page for those documents. An envelope whose
			// horizon was never recorded is left alone — waiting is recoverable,
			// deleting early is not.
			return terminalStatus(e.Status) &&
				!e.RetentionUntil.IsZero() && e.RetentionUntil.Before(w.TerminalBefore)
		}, batch) {
			m.drop(e.ID)
			out.TerminalDeleted++
		}
		for _, e := range m.envelopes {
			if terminalStatus(e.Status) && e.RetentionUntil.IsZero() {
				out.AwaitingHorizon++
			}
		}
	}

	if !w.DraftBefore.IsZero() {
		for _, e := range m.sweepCandidates(func(e *Envelope) bool {
			return e.Status == "draft" && e.CreatedAt.Before(w.DraftBefore)
		}, batch) {
			m.drop(e.ID)
			out.DraftsDeleted++
		}
	}

	return out, nil
}

// ListUnsettledTerminal returns terminal envelopes with no recorded horizon, with
// the documents each is waiting on — the in-memory twin of the repair list.
func (m *Memory) ListUnsettledTerminal(_ context.Context, limit int) ([]UnsettledEnvelope, error) {
	if limit < 0 {
		return nil, ErrInvalid
	}
	if limit == 0 {
		limit = defaultSweepBatch
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]UnsettledEnvelope, 0)
	for _, e := range m.sweepCandidates(func(e *Envelope) bool {
		return terminalStatus(e.Status) && e.RetentionUntil.IsZero()
	}, limit) {
		docs := make([]string, 0, len(m.docs[e.ID]))
		for _, d := range m.docs[e.ID] {
			docs = append(docs, d.DocumentID)
		}
		out = append(out, UnsettledEnvelope{ID: e.ID, Documents: docs})
	}

	return out, nil
}

// sweepCandidates returns up to limit envelopes matching pick, oldest id first so a
// capped pass drains a backlog deterministically. Caller holds the lock.
func (m *Memory) sweepCandidates(pick func(*Envelope) bool, limit int) []*Envelope {
	ids := make([]string, 0, len(m.envelopes))
	for id, e := range m.envelopes {
		if pick(e) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}

	out := make([]*Envelope, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.envelopes[id])
	}

	return out
}

// drop removes an envelope and everything that hangs off it — the in-memory stand-in
// for the schema's cascade. Caller holds the lock.
func (m *Memory) drop(id string) {
	delete(m.envelopes, id)
	delete(m.slots, id)
	delete(m.docs, id)
}

// terminalStatus reports whether a status is an end state, so the envelope has no
// remaining purpose to serve.
func terminalStatus(status string) bool {
	switch status {
	case "completed", "declined", "cancelled", "expired":
		return true
	default:
		return false
	}
}

// Ping always succeeds (in-memory).
func (m *Memory) Ping(_ context.Context) error { return nil }

// Close is a no-op.
func (m *Memory) Close() {}

// canRead reports whether the caller may read the envelope: the owner always may; a
// participant may read a non-draft envelope in which a slot's identity_ref matches their
// authenticated serial. An empty callerSerial never matches a participant. Caller holds
// the lock.
func (m *Memory) canRead(e *Envelope, owner, callerSerial string) bool {
	if e.Owner == owner {
		return true
	}
	if callerSerial == "" || e.Status == "draft" {
		return false
	}
	for _, s := range m.slots[e.ID] {
		if s.IdentityRef == callerSerial {
			return true
		}
	}

	return false
}

// canActOnSlot reports whether the caller may act on this specific slot: the owner
// always may; a participant may act only on the slot whose identity_ref matches their
// authenticated serial, on a non-draft envelope. An empty callerSerial never matches a
// participant.
func canActOnSlot(e *Envelope, slot *Slot, owner, callerSerial string) bool {
	if e.Owner == owner {
		return true
	}

	return callerSerial != "" && e.Status != "draft" && slot.IdentityRef == callerSerial
}

// slotTurn reports whether it is this slot's turn to sign under the envelope's order
// policy: always so for a parallel envelope; for a sequential one, only once every
// lower-order slot is signed. The signer inbox uses it to tell "your turn" from "waiting
// for earlier signers". Caller holds the lock.
func (m *Memory) slotTurn(e *Envelope, slot *Slot) bool {
	if e.OrderPolicy == "parallel" {
		return true
	}
	for _, s := range m.slots[e.ID] {
		if s.OrderIndex < slot.OrderIndex && s.Status != "signed" {
			return false
		}
	}

	return true
}

// findSlot returns the slot with the given id in the envelope, or nil. Caller
// holds the lock.
func (m *Memory) findSlot(envelopeID, slotID string) *Slot {
	for _, s := range m.slots[envelopeID] {
		if s.ID == slotID {
			return s
		}
	}

	return nil
}

// sortedSlots returns the envelope's slots ordered by order_index. Caller holds
// the lock.
func (m *Memory) sortedSlots(envelopeID string) []*Slot {
	out := make([]*Slot, len(m.slots[envelopeID]))
	copy(out, m.slots[envelopeID])
	sort.SliceStable(out, func(i, j int) bool { return out[i].OrderIndex < out[j].OrderIndex })

	return out
}

// allSigned reports whether every slot in the envelope is signed. An envelope with
// no slots is not considered complete. Caller holds the lock.
func (m *Memory) allSigned(envelopeID string) bool {
	slots := m.slots[envelopeID]
	if len(slots) == 0 {
		return false
	}
	for _, s := range slots {
		if s.Status != "signed" {
			return false
		}
	}

	return true
}

// converged reports whether every signed slot's produced container is the same one
// (at most one distinct non-empty signed_doc_ref) — i.e. the co-signatures merged
// into one shared container rather than diverging into separate ones. Caller holds
// the lock.
func (m *Memory) converged(envelopeID string) bool {
	seen := ""
	for _, s := range m.slots[envelopeID] {
		if s.Status != "signed" || s.SignedDocRef == "" {
			continue
		}
		if seen == "" {
			seen = s.SignedDocRef
		} else if s.SignedDocRef != seen {
			return false
		}
	}

	return true
}

// validStatus reports whether s is a permitted envelope status.
func validStatus(s string) bool {
	switch s {
	case "draft", "sent", "in_progress", "completed", "declined", "expired", "cancelled":
		return true
	default:
		return false
	}
}
