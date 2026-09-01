package store

import (
	"context"
	"testing"
	"time"

	"github.com/go-quicktest/qt"
)

// age rewrites an envelope's clocks directly, because a test cannot wait a week for
// a retention window to pass. Zero leaves a clock alone. `horizon` is the instant the
// envelope's documents stop being downloadable — what the keep window is measured
// from, and normally recorded at the terminal transition from the document service.
func age(t *testing.T, m *Memory, id string, created, horizon time.Time) {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.envelopes[id]
	qt.Assert(t, qt.IsTrue(ok))
	if !created.IsZero() {
		e.CreatedAt = created
	}
	if !horizon.IsZero() {
		e.RetentionUntil = horizon
	}
}

// A finished envelope past its keep window is deleted, and everything hanging off
// it goes too — that removal is what takes the signer's identity and name with it.
func TestSweepDeletesTerminalPastKeepWindow(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, envID, "user-a", "completed", view.Envelope.Version)
	qt.Assert(t, qt.IsNil(err))
	age(t, m, envID, time.Time{}, time.Now().UTC().Add(-40*24*time.Hour))

	res, err := m.SweepRetention(ctx, RetentionWindow{
		TerminalBefore: time.Now().UTC().Add(-9 * 24 * time.Hour),
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.TerminalDeleted, 1))

	_, err = m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.ErrorIs(err, ErrNotFound))
	qt.Assert(t, qt.Equals(len(m.slots[envID]), 0))
	qt.Assert(t, qt.Equals(len(m.docs[envID]), 0))
}

// Inside the keep window it stays. The window is the whole control — without this
// the sweep would just be "delete everything finished".
func TestSweepKeepsTerminalInsideKeepWindow(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, envID, "user-a", "completed", view.Envelope.Version)
	qt.Assert(t, qt.IsNil(err))
	// Its documents stopped being downloadable an hour ago — well inside the window.
	age(t, m, envID, time.Time{}, time.Now().UTC().Add(-time.Hour))

	res, err := m.SweepRetention(ctx, RetentionWindow{
		TerminalBefore: time.Now().UTC().Add(-9 * 24 * time.Hour),
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.TerminalDeleted, 0))

	_, err = m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
}

// A finished envelope whose horizon was never recorded is NEVER deleted, however
// old — the sweep waits for an answer instead of guessing, because a guess removes
// a tracking page while its document is still downloadable. It is counted, so the
// waiting is visible rather than an absence nobody notices.
func TestSweepNeverDeletesTerminalWithNoHorizon(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, envID, "user-a", "completed", view.Envelope.Version)
	qt.Assert(t, qt.IsNil(err))

	res, err := m.SweepRetention(ctx, RetentionWindow{
		TerminalBefore: time.Now().UTC().Add(-9 * 24 * time.Hour),
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.TerminalDeleted, 0))
	qt.Assert(t, qt.Equals(res.AwaitingHorizon, 1))

	_, err = m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))

	// Once the horizon is recorded, the very same sweep takes it.
	qt.Assert(t, qt.IsNil(m.SetRetentionUntil(ctx, envID, time.Now().UTC().Add(-40*24*time.Hour))))
	res, err = m.SweepRetention(ctx, RetentionWindow{
		TerminalBefore: time.Now().UTC().Add(-9 * 24 * time.Hour),
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.TerminalDeleted, 1))
	qt.Assert(t, qt.Equals(res.AwaitingHorizon, 0))
}

// An open envelope past its own deadline closes as expired, is reported so the
// caller can announce it, and its version moves so an actor holding the pre-expiry
// version cannot transition it back.
func TestSweepExpiresLapsedEnvelope(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{
		Owner:  "user-a",
		Expiry: time.Now().UTC().Add(-3 * 24 * time.Hour).Format(time.RFC3339),
	})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, created.ID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	res, err := m.SweepRetention(ctx, RetentionWindow{ExpireBefore: time.Now().UTC()})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.ExpiredCount, 1))
	qt.Assert(t, qt.DeepEquals(res.Expired, []string{created.ID}))

	view, err := m.GetEnvelope(ctx, created.ID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "expired"))

	// The stale compare-and-set must lose: version 1 was the "sent" transition.
	_, err = m.ApplyTransition(ctx, created.ID, "user-a", "in_progress", 1)
	qt.Assert(t, qt.ErrorIs(err, ErrConflict))
}

// An envelope whose deadline is still ahead is untouched, and one with no deadline
// at all never expires on its own.
func TestSweepLeavesLiveAndOpenEndedEnvelopes(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	live, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{
		Owner:  "user-a",
		Expiry: time.Now().UTC().Add(3 * 24 * time.Hour).Format(time.RFC3339),
	})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, live.ID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	openEnded, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "user-a"})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, openEnded.ID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))

	res, err := m.SweepRetention(ctx, RetentionWindow{ExpireBefore: time.Now().UTC()})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.ExpiredCount, 0))

	for _, id := range []string{live.ID, openEnded.ID} {
		view, err := m.GetEnvelope(ctx, id, "user-a", "")
		qt.Assert(t, qt.IsNil(err))
		qt.Assert(t, qt.Equals(view.Envelope.Status, "sent"))
	}
}

// THE CASE THAT MATTERS: an envelope expired by this very sweep must survive it.
// Its horizon is recorded at the terminal transition, so during the sweep that
// expires it there is none yet, and an envelope with no horizon is never deleted —
// the difference between "the tracking page went away after the grace period" and
// "it vanished the moment it lapsed".
func TestSweepDoesNotDeleteWhatItJustExpired(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{
		Owner:  "user-a",
		Expiry: time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339),
	})
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, created.ID, "user-a", "sent", 0)
	qt.Assert(t, qt.IsNil(err))
	// No horizon: it has never been terminal, so nothing has recorded one. That is
	// precisely why the delete stage cannot reach it in the same pass that expires it.
	now := time.Now().UTC()
	res, err := m.SweepRetention(ctx, RetentionWindow{
		ExpireBefore:   now,
		TerminalBefore: now.Add(-9 * 24 * time.Hour),
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.ExpiredCount, 1))
	qt.Assert(t, qt.Equals(res.TerminalDeleted, 0))

	view, err := m.GetEnvelope(ctx, created.ID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "expired"))
}

// Drafts leave on their own, longer clock, judged from creation — a draft is
// abandoned when nobody ever sent it.
func TestSweepDeletesAbandonedDrafts(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	old, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "user-a"})
	qt.Assert(t, qt.IsNil(err))
	fresh, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "user-a"})
	qt.Assert(t, qt.IsNil(err))
	age(t, m, old.ID, time.Now().UTC().Add(-90*24*time.Hour), time.Time{})

	res, err := m.SweepRetention(ctx, RetentionWindow{
		DraftBefore: time.Now().UTC().Add(-30 * 24 * time.Hour),
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.DraftsDeleted, 1))

	_, err = m.GetEnvelope(ctx, old.ID, "user-a", "")
	qt.Assert(t, qt.ErrorIs(err, ErrNotFound))
	_, err = m.GetEnvelope(ctx, fresh.ID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
}

// A stage whose instant is absent does not run, so a deployment can enable only the
// parts it wants.
func TestSweepSkipsStagesWithNoInstant(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)
	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, envID, "user-a", "completed", view.Envelope.Version)
	qt.Assert(t, qt.IsNil(err))
	age(t, m, envID, time.Time{}, time.Now().UTC().Add(-40*24*time.Hour))

	// Only the draft stage is named, so the long-finished envelope is untouched.
	res, err := m.SweepRetention(ctx, RetentionWindow{
		DraftBefore: time.Now().UTC().Add(-30 * 24 * time.Hour),
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.TerminalDeleted, 0))
	qt.Assert(t, qt.Equals(res.ExpiredCount, 0))

	_, err = m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
}

// Fail closed: a sweep naming no window at all is a misconfiguration, not a silent
// no-op reported as success.
func TestSweepWithNoWindowIsRefused(t *testing.T) {
	m := NewMemory()

	_, err := m.SweepRetention(context.Background(), RetentionWindow{Batch: 10})
	qt.Assert(t, qt.ErrorIs(err, ErrInvalid))
}

// The batch caps one pass, so a backlog drains over successive sweeps rather than
// one long hold on the table.
func TestSweepBatchCapsOnePass(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()

	for i := 0; i < 3; i++ {
		created, err := m.CreateEnvelope(ctx, CreateEnvelopeInput{Owner: "user-a"})
		qt.Assert(t, qt.IsNil(err))
		age(t, m, created.ID, time.Now().UTC().Add(-90*24*time.Hour), time.Time{})
	}

	window := RetentionWindow{DraftBefore: time.Now().UTC().Add(-30 * 24 * time.Hour), Batch: 2}
	res, err := m.SweepRetention(ctx, window)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.DraftsDeleted, 2))

	res, err = m.SweepRetention(ctx, window)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.DraftsDeleted, 1))

	res, err = m.SweepRetention(ctx, window)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(res.DraftsDeleted, 0))
}

// Total is what the task reads to decide whether anything happened, so it must add
// the three stages rather than any one of them.
func TestSweepResultTotal(t *testing.T) {
	var nilResult *SweepResult
	qt.Assert(t, qt.Equals(nilResult.Total(), 0))

	res := &SweepResult{ExpiredCount: 2, TerminalDeleted: 3, DraftsDeleted: 4}
	qt.Assert(t, qt.Equals(res.Total(), 9))
}

// An expiry the caller cannot have meant is refused at create rather than stored as
// a silently absent deadline.
func TestCreateRejectsUnparsableExpiry(t *testing.T) {
	m := NewMemory()

	_, err := m.CreateEnvelope(context.Background(), CreateEnvelopeInput{
		Owner:  "user-a",
		Expiry: "next tuesday",
	})
	qt.Assert(t, qt.ErrorIs(err, ErrInvalid))
}

// The repair list is the counterpart of AwaitingHorizon: without it that count could
// only grow, because nothing would ever be able to find the rows it counts.
func TestListUnsettledTerminalFindsWhatTheSweepCannotJudge(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, envID, "user-a", "completed", view.Envelope.Version)
	qt.Assert(t, qt.IsNil(err))

	rows, err := m.ListUnsettledTerminal(ctx, 10)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(rows), 1))
	qt.Assert(t, qt.Equals(rows[0].ID, envID))
	// The documents come with it: the caller needs them to do the repair, and a
	// second round trip per envelope over a backlog is the difference between a
	// sweep and an outage.
	qt.Assert(t, qt.DeepEquals(rows[0].Documents, []string{"doc-1"}))

	// Once settled it leaves the list — otherwise the repair would never terminate.
	qt.Assert(t, qt.IsNil(m.SetRetentionUntil(ctx, envID, time.Now().UTC())))
	rows, err = m.ListUnsettledTerminal(ctx, 10)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(rows), 0))
}

// An envelope still in flight is not owed a horizon, so it is not on the list — the
// list is a repair queue, not an inventory of nulls.
func TestListUnsettledTerminalIgnoresOpenEnvelopes(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, _ := draftWithDocAndSlots(t, m, "user-a", false, 1)

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, envID, "user-a", "sent", view.Envelope.Version)
	qt.Assert(t, qt.IsNil(err))

	rows, err := m.ListUnsettledTerminal(ctx, 10)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(rows), 0))
}
