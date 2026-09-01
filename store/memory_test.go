package store

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/go-quicktest/qt"
)

// draftWithDocAndSlots creates a draft envelope for owner with one document and the
// given number of slots, returning the envelope id and the slot ids (in the order
// created). It uses sequential ordering when sequential is true.
func draftWithDocAndSlots(t *testing.T, m *Memory, owner string, sequential bool, slots int) (string, []string) {
	t.Helper()
	ctx := context.Background()

	policy := "parallel"
	if sequential {
		policy = "sequential"
	}
	created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: owner, Title: "t", OrderPolicy: policy})
	qt.Assert(t, qt.IsNil(err))

	qt.Assert(t, qt.IsNil(m.AttachDocument(ctx, created.ID, owner, "doc-1", "hash-1")))

	ids := make([]string, 0, slots)
	for i := 0; i < slots; i++ {
		id, err := m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: owner, OrderIndex: i + 1})
		qt.Assert(t, qt.IsNil(err))
		ids = append(ids, id)
	}

	return created.ID, ids
}

// A draft envelope with a document + a slot can be sent; the version is bumped.
func TestCreateAttachSlotSend(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "draft"))
	qt.Assert(t, qt.Equals(len(view.Documents), 1))
	qt.Assert(t, qt.Equals(len(view.Slots), 1))

	res, err := m.ApplyTransition(ctx, envID, "user-a", "sent", view.Envelope.Version)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.Status, "sent"))
	qt.Assert(t, qt.Equals(res.Version, view.Envelope.Version+1))
}

// Another user cannot read an envelope they do not own — owner-filtered to a miss.
func TestOwnerFilterNotFound(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)

	_, err := m.GetEnvelope(ctx, envID, "user-b", "")
	qt.Assert(t, qt.ErrorIs(err, ErrNotFound))
}

// A transition on a stale version is rejected with a conflict (compare-and-set).
func TestTransitionConflict(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)

	// First transition with version 0 succeeds, bumping to 1.
	_, err := m.ApplyTransition(ctx, envID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	// A second transition reusing the now-stale version 0 conflicts.
	_, err = m.ApplyTransition(ctx, envID, "user-a", "cancelled", 0)
	qt.Assert(t, qt.ErrorIs(err, ErrConflict))
}

// Sequential ordering: slot 2 is not eligible until slot 1 is signed; parallel
// opens both.
func TestSlotEligibleSequential(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := draftWithDocAndSlots(t, m, "user-a", true, 2)
	_, err := m.ApplyTransition(ctx, envID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	// Slot 1 is eligible; slot 2 is gated on slot 1.
	ok, err := m.SlotEligible(ctx, envID, slots[0], "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsTrue(ok))

	ok, err = m.SlotEligible(ctx, envID, slots[1], "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsFalse(ok))

	// After slot 1 signs, slot 2 becomes eligible.
	_, err = m.MarkSlotSigned(ctx, envID, slots[0], "sig-1", "", "")
	qt.Assert(t, qt.IsNil(err))

	ok, err = m.SlotEligible(ctx, envID, slots[1], "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsTrue(ok))
}

// The slot-signed callback is idempotent (a replay is a no-op) and the envelope
// rolls up to completed once every slot is signed.
func TestMarkSlotSignedIdempotentAndComplete(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := draftWithDocAndSlots(t, m, "user-a", false, 2)
	_, err := m.ApplyTransition(ctx, envID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	// First sign of slot 1 advances the envelope to in_progress.
	res, err := m.MarkSlotSigned(ctx, envID, slots[0], "sig-1", "container-1", "job-1")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.Status, "signed"))
	qt.Assert(t, qt.IsFalse(res.Idempotent))

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "in_progress"))

	// A replayed callback for slot 1 is a no-op success.
	res, err = m.MarkSlotSigned(ctx, envID, slots[0], "sig-1", "container-1", "job-1")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsTrue(res.Idempotent))

	// Signing the last slot completes the envelope.
	_, err = m.MarkSlotSigned(ctx, envID, slots[1], "sig-2", "", "")
	qt.Assert(t, qt.IsNil(err))

	view, err = m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "completed"))
}

// A divergent result — the two slots' signatures landed in DIFFERENT containers
// instead of merging into one shared one — must NOT roll the envelope up to
// completed (a wrong result must never read as done). It stays in_progress for
// reconciliation. (Guarded upstream by one-container-per-chain; this is defense.)
func TestMarkSlotSignedDivergentContainersDoesNotComplete(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := draftWithDocAndSlots(t, m, "user-a", false, 2)
	_, err := m.ApplyTransition(ctx, envID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	_, err = m.MarkSlotSigned(ctx, envID, slots[0], "sig-1", "container-A", "job-1")
	qt.Assert(t, qt.IsNil(err))
	_, err = m.MarkSlotSigned(ctx, envID, slots[1], "sig-2", "container-B", "job-2")
	qt.Assert(t, qt.IsNil(err))

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	// All slots signed, but into two containers → refuse to mark completed.
	qt.Assert(t, qt.Equals(view.Envelope.Status, "in_progress"))
}

// A decline drives the envelope to declined.
func TestDeclineSlot(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := draftWithDocAndSlots(t, m, "user-a", false, 1)
	_, err := m.ApplyTransition(ctx, envID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	res, err := m.DeclineSlot(ctx, envID, slots[0], "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	// The result carries the terminal envelope status + doc ids, so the caller
	// can lift the chains' download freeze without an owner-scoped re-read.
	qt.Assert(t, qt.Equals(res.EnvelopeStatus, "declined"))
	qt.Assert(t, qt.DeepEquals(res.DocIDs, []string{"doc-1"}))

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "declined"))
}

// A duplicate document attach and a duplicate slot order index are rejected.
func TestDuplicateRejected(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "user-a"})
	qt.Assert(t, qt.IsNil(err))

	qt.Assert(t, qt.IsNil(m.AttachDocument(ctx, created.ID, "user-a", "doc-1", "h")))
	qt.Assert(t, qt.ErrorIs(m.AttachDocument(ctx, created.ID, "user-a", "doc-1", "h"), ErrDuplicate))

	_, err = m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: "user-a", OrderIndex: 1})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: "user-a", OrderIndex: 1})
	qt.Assert(t, qt.ErrorIs(err, ErrDuplicate))
}

// A document can only be attached to a draft envelope.
func TestAttachOnlyDraft(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)
	_, err := m.ApplyTransition(ctx, envID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	err = m.AttachDocument(ctx, envID, "user-a", "doc-2", "h2")
	qt.Assert(t, qt.ErrorIs(err, ErrInvalid))
}

// testIDCodeLV returns a Latvian personal identity code in the PNO form a signer
// slot is invited by: the country, a six-digit leading group and a five-digit
// serial, built from one repeated digit so it reads as a placeholder at a glance
// and each test person is told apart by their digit.
//
// It is assembled from those parts at run time rather than written as a literal —
// an identifier-shaped constant in the source is indistinguishable from a
// credential to a secret scanner, and indistinguishable from a real person's code
// to a reader.
func testIDCodeLV(digit int) string {
	d := strconv.Itoa(digit)

	return "PNOLV-" + strings.Repeat(d, 6) + "-" + strings.Repeat(d, 5)
}

// coSignerSerial is a stand-in eIDAS identity code the owner invites a co-signer by.
var coSignerSerial = testIDCodeLV(1)

// sentEnvelopeWithCoSigner creates + sends an envelope owned by owner with two slots:
// slot 0 the owner's (no identity_ref), slot 1 the co-signer invited by coSerial. It
// returns the envelope id and the slot ids.
func sentEnvelopeWithCoSigner(t *testing.T, m *Memory, owner, coSerial string) (string, []string) {
	t.Helper()
	ctx := context.Background()

	created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: owner, Title: "t", OrderPolicy: "parallel"})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsNil(m.AttachDocument(ctx, created.ID, owner, "doc-1", "hash-1")))

	s0, err := m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: owner, OrderIndex: 1})
	qt.Assert(t, qt.IsNil(err))
	s1, err := m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: owner, OrderIndex: 2, IdentityRef: coSerial})
	qt.Assert(t, qt.IsNil(err))

	_, err = m.ApplyTransition(ctx, created.ID, owner, "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	return created.ID, []string{s0, s1}
}

// A co-signer invited by serial can read the sent envelope; the owner reads it without a
// serial; a caller matching no slot — or with no serial — is a 404 (no enumeration).
func TestParticipantReadsSentEnvelope(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := sentEnvelopeWithCoSigner(t, m, "owner-a", coSignerSerial)

	// The co-signer (a different sub) reads it via their matching serial.
	view, err := m.GetEnvelope(ctx, envID, "user-b", coSignerSerial)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "sent"))

	// The owner still reads it without a serial.
	_, err = m.GetEnvelope(ctx, envID, "owner-a", "")
	qt.Assert(t, qt.IsNil(err))

	// A caller matching no slot is a 404.
	_, err = m.GetEnvelope(ctx, envID, "user-c", testIDCodeLV(9))
	qt.Assert(t, qt.ErrorIs(err, ErrNotFound))

	// A non-owner with no serial never matches a participant.
	_, err = m.GetEnvelope(ctx, envID, "user-c", "")
	qt.Assert(t, qt.ErrorIs(err, ErrNotFound))
}

// A co-signer cannot see a draft envelope — it has not been sent to them yet.
func TestParticipantCannotReadDraft(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "owner-a"})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: "owner-a", OrderIndex: 1, IdentityRef: coSignerSerial})
	qt.Assert(t, qt.IsNil(err))

	_, err = m.GetEnvelope(ctx, created.ID, "user-b", coSignerSerial)
	qt.Assert(t, qt.ErrorIs(err, ErrNotFound))
}

// A co-signer may check + act on only their own slot; the owner's slot is a 404 to them.
func TestParticipantActsOnOwnSlotOnly(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := sentEnvelopeWithCoSigner(t, m, "owner-a", coSignerSerial)
	ownerSlot, coSlot := slots[0], slots[1]

	// Eligible on their own slot — yes; on the owner's slot — 404.
	ok, err := m.SlotEligible(ctx, envID, coSlot, "user-b", coSignerSerial)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsTrue(ok))

	_, err = m.SlotEligible(ctx, envID, ownerSlot, "user-b", coSignerSerial)
	qt.Assert(t, qt.ErrorIs(err, ErrNotFound))

	// They can record their signing job on their own slot, not the owner's.
	qt.Assert(t, qt.IsNil(m.SetSlotJob(ctx, envID, coSlot, "user-b", coSignerSerial, "job-co")))
	qt.Assert(t, qt.ErrorIs(m.SetSlotJob(ctx, envID, ownerSlot, "user-b", coSignerSerial, "job-x"), ErrNotFound))
}

// The signer inbox lists exactly the envelopes a co-signer is invited to (matched by
// serial), excludes the owner's own envelopes, and is empty for a stranger serial.
func TestSigningTasksListsParticipantEnvelopes(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := sentEnvelopeWithCoSigner(t, m, "owner-a", coSignerSerial)

	// The co-signer (a different sub) sees exactly their one task: their slot, their turn.
	tasks, err := m.ListSigningTasks(ctx, coSignerSerial, "user-b", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(tasks), 1))
	qt.Assert(t, qt.Equals(tasks[0].Envelope.ID, envID))
	qt.Assert(t, qt.Equals(tasks[0].Slot.ID, slots[1]))
	qt.Assert(t, qt.IsTrue(tasks[0].YourTurn)) // parallel ⇒ always the caller's turn
	// The task's envelope carries its attached document ids — an inbox consumer composing
	// envelopes and standalone documents must know which chains the invitation covers.
	qt.Assert(t, qt.DeepEquals(tasks[0].Envelope.DocIDs, []string{"doc-1"}))

	// The owner does NOT see it in their signer inbox — they own it (it lists elsewhere).
	tasks, err = m.ListSigningTasks(ctx, coSignerSerial, "owner-a", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(tasks), 0))

	// A stranger serial matches no slot.
	tasks, err = m.ListSigningTasks(ctx, testIDCodeLV(9), "user-c", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(tasks), 0))
}

// A draft envelope never appears in the inbox; an empty serial is rejected.
func TestSigningTasksDraftAndEmptySerial(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "owner-a"})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: "owner-a", OrderIndex: 1, IdentityRef: coSignerSerial})
	qt.Assert(t, qt.IsNil(err))

	// Still a draft → not yet sent to the co-signer → not in the inbox.
	tasks, err := m.ListSigningTasks(ctx, coSignerSerial, "user-b", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(tasks), 0))

	// No serial at all is a domain error (a caller with no identity has nothing to match).
	_, err = m.ListSigningTasks(ctx, "", "user-b", "", 0)
	qt.Assert(t, qt.ErrorIs(err, ErrInvalid))
}

// Once the co-signer signs (or declines) their slot, it leaves the "awaiting" inbox.
func TestSigningTasksDropsResolvedSlot(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := sentEnvelopeWithCoSigner(t, m, "owner-a", coSignerSerial)

	tasks, err := m.ListSigningTasks(ctx, coSignerSerial, "user-b", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(tasks), 1))

	// The co-signer's slot is signed (by the orchestrator callback) → no longer awaiting them.
	_, err = m.MarkSlotSigned(ctx, envID, slots[1], "sig-co", "", "")
	qt.Assert(t, qt.IsNil(err))

	tasks, err = m.ListSigningTasks(ctx, coSignerSerial, "user-b", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(tasks), 0))
}

// In a sequential envelope, the inbox reports your_turn=false until the earlier slot signs.
func TestSigningTasksYourTurnSequential(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "owner-a", OrderPolicy: "sequential"})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsNil(m.AttachDocument(ctx, created.ID, "owner-a", "doc-1", "hash-1")))
	s0, err := m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: "owner-a", OrderIndex: 1})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: "owner-a", OrderIndex: 2, IdentityRef: coSignerSerial})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, created.ID, "owner-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	// The co-signer's slot is in the inbox, but it is not their turn yet (slot 0 unsigned).
	tasks, err := m.ListSigningTasks(ctx, coSignerSerial, "user-b", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(tasks), 1))
	qt.Assert(t, qt.IsFalse(tasks[0].YourTurn))

	// After the earlier slot signs, it becomes their turn.
	_, err = m.MarkSlotSigned(ctx, created.ID, s0, "sig-0", "", "")
	qt.Assert(t, qt.IsNil(err))

	tasks, err = m.ListSigningTasks(ctx, coSignerSerial, "user-b", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(tasks), 1))
	qt.Assert(t, qt.IsTrue(tasks[0].YourTurn))
}

// A co-signer may decline their own slot (driving the envelope to declined) but not the
// owner's.
func TestParticipantDeclinesOwnSlot(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := sentEnvelopeWithCoSigner(t, m, "owner-a", coSignerSerial)

	_, err := m.DeclineSlot(ctx, envID, slots[0], "user-b", coSignerSerial)
	qt.Assert(t, qt.ErrorIs(err, ErrNotFound))
	res, err := m.DeclineSlot(ctx, envID, slots[1], "user-b", coSignerSerial)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.EnvelopeStatus, "declined"))

	view, err := m.GetEnvelope(ctx, envID, "owner-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "declined"))
}

// ListEnvelopes carries the progress projection: created time, slot/signed counts, and
// the owner's turn — true only while the owner has an outstanding, order-eligible slot of
// their own (identity_ref NULL), and false once that slot signs.
func TestListEnvelopesProjection(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := sentEnvelopeWithCoSigner(t, m, "owner-a", coSignerSerial) // parallel: owner slot [0] + co-signer slot [1]

	list, err := m.ListEnvelopes(ctx, "owner-a", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(list), 1))
	qt.Assert(t, qt.Equals(list[0].ID, envID))
	qt.Assert(t, qt.Equals(list[0].SlotCount, 2))
	qt.Assert(t, qt.Equals(list[0].SignedCount, 0))
	qt.Assert(t, qt.IsTrue(list[0].YourTurn)) // the owner's own slot is outstanding
	qt.Assert(t, qt.IsFalse(list[0].CreatedAt.IsZero()))
	qt.Assert(t, qt.DeepEquals(list[0].DocIDs, []string{"doc-1"})) // attached doc refs ride the summary

	// The owner signs their own slot → the signed count rises and it is no longer their turn.
	_, err = m.MarkSlotSigned(ctx, envID, slots[0], "sig-owner", "", "")
	qt.Assert(t, qt.IsNil(err))
	list, err = m.ListEnvelopes(ctx, "owner-a", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(list[0].SignedCount, 1))
	qt.Assert(t, qt.IsFalse(list[0].YourTurn))
}

// CaptureSignerName writes a participant's name onto their OWN slot (owner or serial-matched),
// write-once, and it surfaces in the read; a second write and an unentitled caller are silent
// no-ops (never an error).
func TestCaptureSignerName(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, slots := sentEnvelopeWithCoSigner(t, m, "owner-a", coSignerSerial) // slots[0]=owner (NULL), slots[1]=co-signer

	qt.Assert(t, qt.IsNil(m.CaptureSignerName(ctx, envID, slots[0], "owner-a", "", "Alice Owner")))
	qt.Assert(t, qt.IsNil(m.CaptureSignerName(ctx, envID, slots[1], "user-b", coSignerSerial, "Bob Cosigner")))

	names := func() map[string]string {
		view, err := m.GetEnvelope(ctx, envID, "owner-a", "")
		qt.Assert(t, qt.IsNil(err))
		out := map[string]string{}
		for _, s := range view.Slots {
			out[s.ID] = s.SignerName
		}

		return out
	}
	got := names()
	qt.Assert(t, qt.Equals(got[slots[0]], "Alice Owner"))
	qt.Assert(t, qt.Equals(got[slots[1]], "Bob Cosigner"))

	// Write-once: a second capture does not overwrite. An unentitled caller is a no-op.
	qt.Assert(t, qt.IsNil(m.CaptureSignerName(ctx, envID, slots[0], "owner-a", "", "Someone Else")))
	qt.Assert(t, qt.IsNil(m.CaptureSignerName(ctx, envID, slots[1], "user-c", testIDCodeLV(9), "Impostor")))
	got = names()
	qt.Assert(t, qt.Equals(got[slots[0]], "Alice Owner"))
	qt.Assert(t, qt.Equals(got[slots[1]], "Bob Cosigner"))
}

// The owner's turn is scoped to the owner's OWN slot: a draft is never their turn (not yet
// actionable), and a sent envelope whose only slot belongs to a co-signer is not either.
func TestListEnvelopesOwnerTurnScope(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	// Draft with the owner's own slot → not actionable → not their turn yet.
	_, _ = draftWithDocAndSlots(t, m, "owner-a", false, 1)
	list, err := m.ListEnvelopes(ctx, "owner-a", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(list), 1))
	qt.Assert(t, qt.IsFalse(list[0].YourTurn))

	// A different owner, sent, whose only slot is a co-signer's → actionable but not the owner's turn.
	created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "owner-b", OrderPolicy: "parallel"})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsNil(m.AttachDocument(ctx, created.ID, "owner-b", "doc-1", "hash-1")))
	_, err = m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: "owner-b", OrderIndex: 1, IdentityRef: coSignerSerial})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, created.ID, "owner-b", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	list, err = m.ListEnvelopes(ctx, "owner-b", "", 0)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(list), 1))
	qt.Assert(t, qt.Equals(list[0].SlotCount, 1))
	qt.Assert(t, qt.IsFalse(list[0].YourTurn))
}

// The edition signs between two parties: a third SIGNER is refused. The refusal is
// its own sentinel, so a route can answer 422 with a code of its own rather than
// collapsing into the generic invalid case.
func TestThirdSignerRefused(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 2)

	_, err := m.AddSlot(ctx, AddSlotInput{EnvelopeID: envID, Owner: "user-a", OrderIndex: 3})
	qt.Assert(t, qt.ErrorIs(err, ErrSlotLimit))
}

// Only signers count. An approver or an observer beside two signers is not a third
// party to the signature, and refusing them would withdraw something that works.
func TestNonSignerSlotsAreNotCounted(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 2)

	_, err := m.AddSlot(ctx, AddSlotInput{EnvelopeID: envID, Owner: "user-a", OrderIndex: 3, Role: "observer"})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.AddSlot(ctx, AddSlotInput{EnvelopeID: envID, Owner: "user-a", OrderIndex: 4, Role: "approver"})
	qt.Assert(t, qt.IsNil(err))

	// …and the signer limit still holds with those in place.
	_, err = m.AddSlot(ctx, AddSlotInput{EnvelopeID: envID, Owner: "user-a", OrderIndex: 5})
	qt.Assert(t, qt.ErrorIs(err, ErrSlotLimit))
}

// The limit counts rows; it never bounds the order index. Callers number their slots
// from 0 or from 1 as they please — the wizard sends 0,1 and the E2E harness sends
// 1,2 — and both are two signers. An index bound would refuse the second of these,
// which works in production today.
func TestLimitCountsRowsNotOrderIndex(t *testing.T) {
	ctx := context.Background()

	for _, indices := range [][]int{{0, 1}, {1, 2}, {4, 9}} {
		m := NewMemory()
		created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "user-a", Title: "t", OrderPolicy: "parallel"})
		qt.Assert(t, qt.IsNil(err))
		for _, idx := range indices {
			_, err := m.AddSlot(ctx, AddSlotInput{EnvelopeID: created.ID, Owner: "user-a", OrderIndex: idx})
			qt.Assert(t, qt.IsNil(err))
		}
		view, err := m.GetEnvelope(ctx, created.ID, "user-a", "")
		qt.Assert(t, qt.IsNil(err))
		qt.Assert(t, qt.Equals(len(view.Slots), 2))
	}
}

// The boundary from the other side: the guard is the count, not the number two, so a
// build carrying a different edition count admits the slot the default refuses.
func TestSignerLimitBoundaryBothWays(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 2)

	_, err := m.AddSlot(ctx, AddSlotInput{EnvelopeID: envID, Owner: "user-a", OrderIndex: 3})
	qt.Assert(t, qt.ErrorIs(err, ErrSlotLimit))

	restore := signerSlotLimit
	signerSlotLimit = 3
	defer func() { signerSlotLimit = restore }()

	_, err = m.AddSlot(ctx, AddSlotInput{EnvelopeID: envID, Owner: "user-a", OrderIndex: 3})
	qt.Assert(t, qt.IsNil(err))
}
