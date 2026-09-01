package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-quicktest/qt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gmb-lib/go-sec-events/secevents"

	"github.com/signbyte/envelope/audit"
	"github.com/signbyte/envelope/store"
)

// recordingStore is a Store that only answers SweepRetention: it hands back scripted
// passes and records the windows it was called with, so a test can assert what the
// task asked for as well as what it did with the answer. Every other method panics —
// the task must never reach for one.
type recordingStore struct {
	store.Store

	mu      sync.Mutex
	windows []store.RetentionWindow
	passes  []*store.SweepResult
	err     error
}

func (s *recordingStore) SweepRetention(_ context.Context, w store.RetentionWindow) (*store.SweepResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.windows = append(s.windows, w)
	if s.err != nil {
		return nil, s.err
	}
	if len(s.passes) == 0 {
		return &store.SweepResult{Expired: []string{}}, nil
	}

	res := s.passes[0]
	s.passes = s.passes[1:]

	return res, nil
}

func (s *recordingStore) calls() []store.RetentionWindow {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]store.RetentionWindow(nil), s.windows...)
}

func TestRetentionTaskName(t *testing.T) {
	qt.Assert(t, qt.Equals(NewRetentionTask(RetentionConfig{}).Name(), "envelope-retention"))
}

// The window the task builds is where the configured clocks turn into instants: the
// keep window counts back from now, drafts count back from their own longer window,
// and expiring is judged at the clock itself.
func TestRunOnceBuildsWindowFromConfiguredClocks(t *testing.T) {
	s := &recordingStore{}
	task := NewRetentionTask(RetentionConfig{
		Store:    s,
		Batch:    50,
		Grace:    9 * 24 * time.Hour,
		DraftTTL: 30 * 24 * time.Hour,
	}).(*retentionTask)

	before := time.Now().UTC()
	task.runOnce(context.Background())
	after := time.Now().UTC()

	calls := s.calls()
	qt.Assert(t, qt.Equals(len(calls), 1))
	w := calls[0]
	qt.Assert(t, qt.Equals(w.Batch, 50))
	qt.Assert(t, qt.IsFalse(w.ExpireBefore.Before(before)))
	qt.Assert(t, qt.IsFalse(w.ExpireBefore.After(after)))
	qt.Assert(t, qt.IsTrue(w.TerminalBefore.Before(before.Add(-9*24*time.Hour).Add(time.Second))))
	qt.Assert(t, qt.IsTrue(w.DraftBefore.Before(before.Add(-30*24*time.Hour).Add(time.Second))))
}

// A disabled stage must not reach the data layer as a zero instant — it is left out
// of the window entirely, which is what the store turns into "skip this stage".
func TestRunOnceLeavesDisabledStagesUnset(t *testing.T) {
	s := &recordingStore{}
	task := NewRetentionTask(RetentionConfig{Store: s}).(*retentionTask)

	task.runOnce(context.Background())

	w := s.calls()[0]
	qt.Assert(t, qt.IsTrue(w.TerminalBefore.IsZero()))
	qt.Assert(t, qt.IsTrue(w.DraftBefore.IsZero()))
	qt.Assert(t, qt.IsFalse(w.ExpireBefore.IsZero()))
}

// A backlog drains over repeated passes: while a stage keeps filling its batch there
// is more to do, and the task keeps going until none of them does.
func TestRunOnceRepeatsWhileAStageFillsItsBatch(t *testing.T) {
	s := &recordingStore{passes: []*store.SweepResult{
		{TerminalDeleted: 2},
		{DraftsDeleted: 2},
		{ExpiredCount: 1, Expired: []string{"env-1"}},
	}}
	task := NewRetentionTask(RetentionConfig{Store: s, Batch: 2}).(*retentionTask)

	task.runOnce(context.Background())

	qt.Assert(t, qt.Equals(len(s.calls()), 3))
}

// A pass that fills nothing is the end of the sweep — the task must not keep asking.
func TestRunOnceStopsAfterAnUnderfilledPass(t *testing.T) {
	s := &recordingStore{passes: []*store.SweepResult{{TerminalDeleted: 1}}}
	task := NewRetentionTask(RetentionConfig{Store: s, Batch: 500}).(*retentionTask)

	task.runOnce(context.Background())

	qt.Assert(t, qt.Equals(len(s.calls()), 1))
}

// A failing sweep gives up for this tick rather than spinning; the next tick tries
// again, so a transient database problem costs one interval, not the service.
func TestRunOnceStopsOnError(t *testing.T) {
	s := &recordingStore{err: errors.New("store unavailable")}
	task := NewRetentionTask(RetentionConfig{Store: s, Batch: 10}).(*retentionTask)

	task.runOnce(context.Background())

	qt.Assert(t, qt.Equals(len(s.calls()), 1))
}

// A task wired without a batch falls back to a sane step rather than asking for an
// unbounded pass.
func TestRunOnceDefaultsBatchWhenUnset(t *testing.T) {
	s := &recordingStore{}
	task := NewRetentionTask(RetentionConfig{Store: s}).(*retentionTask)

	task.runOnce(context.Background())

	qt.Assert(t, qt.Equals(s.calls()[0].Batch, defaultBatch))
}

// Every expiry is announced, so whoever notifies the parties learns the envelope
// closed on its own deadline. A nil recorder is the unconfigured case and must not
// panic the sweep.
func TestRunOnceAnnouncesEachExpiryWithoutARecorder(t *testing.T) {
	s := &recordingStore{passes: []*store.SweepResult{
		{ExpiredCount: 2, Expired: []string{"env-1", "env-2"}},
	}}
	task := NewRetentionTask(RetentionConfig{Store: s, Batch: 500}).(*retentionTask)

	task.runOnce(context.Background())

	qt.Assert(t, qt.Equals(len(s.calls()), 1))
}

// securityEvents returns the security records a sweep wrote, with their attributes.
func securityEvents(logs *observer.ObservedLogs) []map[string]any {
	out := []map[string]any{}
	for _, e := range logs.FilterMessage("security_event").All() {
		for _, f := range e.Context {
			if f.Key == "attributes" {
				if m, ok := f.Interface.(map[string]any); ok {
					out = append(out, m)
				}
			}
		}
	}

	return out
}

// The erasure record is written ONCE per sweep and carries the totals across every
// pass. A backlog that drains over three passes is one act of policy, not three —
// counting it three times would overstate the erasure in the one stream that is
// supposed to report it accurately.
func TestRunOnceRecordsOneErasureRecordWithTheSweepTotals(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	s := &recordingStore{passes: []*store.SweepResult{
		{TerminalDeleted: 2},
		{TerminalDeleted: 2, DraftsDeleted: 1},
		{ExpiredCount: 1, Expired: []string{"env-1"}},
	}}
	task := NewRetentionTask(RetentionConfig{
		Store:    s,
		Audit:    audit.New(secevents.NewEmitter(secevents.NewLogSinkFor(zap.New(core))), nil, nil, "", zap.New(core)),
		Batch:    2,
		Grace:    216 * time.Hour,
		DraftTTL: 720 * time.Hour,
	}).(*retentionTask)

	task.runOnce(context.Background())

	events := securityEvents(logs)
	qt.Assert(t, qt.Equals(len(events), 1))
	qt.Assert(t, qt.Equals(events[0]["expired"], 1))
	qt.Assert(t, qt.Equals(events[0]["terminal_deleted"], 4))
	qt.Assert(t, qt.Equals(events[0]["drafts_deleted"], 1))
	qt.Assert(t, qt.Equals(events[0]["erased"], 5))
	qt.Assert(t, qt.Equals(events[0]["grace"], "216h0m0s"))
}

// A sweep that erased nothing writes no erasure record — the quiet hour is the
// common case, and an hourly "erased 0" would bury the ones that matter.
func TestRunOnceRecordsNoErasureWhenNothingWasSwept(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	s := &recordingStore{}
	task := NewRetentionTask(RetentionConfig{
		Store: s,
		Audit: audit.New(secevents.NewEmitter(secevents.NewLogSinkFor(zap.New(core))), nil, nil, "", zap.New(core)),
		Batch: 10,
	}).(*retentionTask)

	task.runOnce(context.Background())

	qt.Assert(t, qt.Equals(len(securityEvents(logs)), 0))
}

// A failed sweep must not claim an erasure it never performed: the record is the
// accountability trail, so a pass that died mid-way records nothing rather than
// whatever it had counted before the failure.
func TestRunOnceRecordsNoErasureWhenTheSweepFailed(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	s := &recordingStore{err: errors.New("store unavailable")}
	task := NewRetentionTask(RetentionConfig{
		Store: s,
		Audit: audit.New(secevents.NewEmitter(secevents.NewLogSinkFor(zap.New(core))), nil, nil, "", zap.New(core)),
		Batch: 10,
	}).(*retentionTask)

	task.runOnce(context.Background())

	qt.Assert(t, qt.Equals(len(securityEvents(logs)), 0))
}

// Start runs an initial sweep immediately rather than waiting out the first tick, so
// a restarted service does not leave a backlog sitting for an interval.
func TestStartSweepsImmediatelyAndStopIsSafe(t *testing.T) {
	s := &recordingStore{}
	task := NewRetentionTask(RetentionConfig{Store: s, Interval: time.Hour, Batch: 10})

	qt.Assert(t, qt.IsNil(task.Start(context.Background())))
	defer task.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for len(s.calls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Start did not run an initial sweep in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStopWithoutStartIsSafe(t *testing.T) {
	NewRetentionTask(RetentionConfig{}).Stop() // ticker is nil — must not panic or block
}

// ---------------------------------------------------------------------------
// The repair stage: an envelope the sweep expires must still be able to leave.

// settleDocs is a document service for the repair stage: it answers a fixed
// retention instant, counts the freezes it was asked to lift, and can be made to
// fail so a test can watch what an unreachable document service does to a row.
type settleDocs struct {
	mu       sync.Mutex
	until    time.Time
	live     int
	lifted   []string
	retErr   error
	freezErr error
}

func (d *settleDocs) SetResultFreeze(_ context.Context, id string, frozen bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !frozen {
		d.lifted = append(d.lifted, id)
	}

	return d.freezErr
}

func (d *settleDocs) ChainRetention(_ context.Context, _ string) (time.Time, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.retErr != nil {
		return time.Time{}, 0, d.retErr
	}

	return d.until, d.live, nil
}

func (d *settleDocs) liftedDocs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]string(nil), d.lifted...)
}

// sentEnvelopePastItsDeadline builds a real envelope in the memory store, sends it,
// and puts its deadline in the past — the shape the sweep is meant to notice.
func sentEnvelopePastItsDeadline(t *testing.T, m *store.Memory) string {
	t.Helper()
	ctx := context.Background()

	created, err := m.CreateEnvelope(ctx, store.CreateEnvelopeInput{
		Owner: "user-a", Title: "t", OrderPolicy: "parallel",
		Expiry: time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano),
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsNil(m.AttachDocument(ctx, created.ID, "user-a", "doc-1", "hash-1")))
	_, err = m.AddSlot(ctx, store.AddSlotInput{EnvelopeID: created.ID, Owner: "user-a", OrderIndex: 1})
	qt.Assert(t, qt.IsNil(err))

	view, err := m.GetEnvelope(ctx, created.ID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, created.ID, "user-a", "sent", view.Envelope.Version)
	qt.Assert(t, qt.IsNil(err))

	return created.ID
}

// THE regression: an envelope the sweep expires reaches a terminal state with no
// request behind it, so nothing read its documents — and the delete stage needs a
// horizon. Before the repair stage existed this envelope became permanently
// unretirable: the one case the whole task was built for was the one it could not
// finish. The assertion is the full arc — expired, then settled, then gone.
func TestSweepRetiresAnEnvelopeItExpiredItself(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	id := sentEnvelopePastItsDeadline(t, m)

	docs := &settleDocs{until: time.Now().UTC().Add(-90 * 24 * time.Hour), live: 1}
	task := &retentionTask{cfg: RetentionConfig{
		Store: m, Documents: docs, Audit: audit.New(nil, nil, nil, "", zap.NewNop()),
		Batch: 10, Grace: time.Hour, DraftTTL: 720 * time.Hour, Logger: zap.NewNop(),
	}}

	// Pass one expires it. Nothing has read its documents yet, so it has no horizon.
	task.runOnce(ctx)
	view, err := m.GetEnvelope(ctx, id, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "expired"))

	// Pass two settles it against its documents and, its horizon now long past,
	// deletes it. Without the repair stage this pass — and every later one — would
	// leave the row exactly where it was.
	task.runOnce(ctx)
	_, err = m.GetEnvelope(ctx, id, "user-a", "")
	qt.Assert(t, qt.ErrorIs(err, store.ErrNotFound))

	// And the freeze taken at send was lifted on the way out, which is the other
	// half of what a terminal transition owes its documents.
	qt.Assert(t, qt.DeepEquals(docs.liftedDocs(), []string{"doc-1"}))
}

// Rows that predate the horizon being recorded at all are the same repair: without
// this, an existing deployment would never reclaim a single one of its finished
// envelopes, which is the storage and personal-data argument the task exists for.
func TestSweepSettlesTerminalRowsThatNeverHadAHorizon(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()

	created, err := m.CreateEnvelope(ctx, store.CreateEnvelopeInput{Owner: "user-a", Title: "t", OrderPolicy: "parallel"})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsNil(m.AttachDocument(ctx, created.ID, "user-a", "doc-9", "hash-9")))
	_, err = m.AddSlot(ctx, store.AddSlotInput{EnvelopeID: created.ID, Owner: "user-a", OrderIndex: 1})
	qt.Assert(t, qt.IsNil(err))
	view, err := m.GetEnvelope(ctx, created.ID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	_, err = m.ApplyTransition(ctx, created.ID, "user-a", "cancelled", view.Envelope.Version)
	qt.Assert(t, qt.IsNil(err))

	docs := &settleDocs{until: time.Now().UTC().Add(-90 * 24 * time.Hour), live: 1}
	task := &retentionTask{cfg: RetentionConfig{
		Store: m, Documents: docs, Audit: audit.New(nil, nil, nil, "", zap.NewNop()),
		Batch: 10, Grace: time.Hour, Logger: zap.NewNop(),
	}}

	task.runOnce(ctx)
	_, err = m.GetEnvelope(ctx, created.ID, "user-a", "")
	qt.Assert(t, qt.ErrorIs(err, store.ErrNotFound))
}

// A document service that cannot answer must leave the row exactly where it was —
// unsettled, and therefore still on the repair list. Waiting is recoverable;
// recording a horizon nobody read would delete a tracking page early.
func TestSettleFailureLeavesTheEnvelopeForTheNextPass(t *testing.T) {
	ctx := context.Background()
	m := store.NewMemory()
	id := sentEnvelopePastItsDeadline(t, m)

	docs := &settleDocs{retErr: errors.New("document service unreachable")}
	task := &retentionTask{cfg: RetentionConfig{
		Store: m, Documents: docs, Audit: audit.New(nil, nil, nil, "", zap.NewNop()),
		Batch: 10, Grace: time.Hour, Logger: zap.NewNop(),
	}}

	task.runOnce(ctx)
	task.runOnce(ctx)

	view, err := m.GetEnvelope(ctx, id, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(view.Envelope.Status, "expired"))

	rows, err := m.ListUnsettledTerminal(ctx, 10)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(rows), 1))
	qt.Assert(t, qt.Equals(rows[0].ID, id))
}

// Wired without a document service the sweep still runs, and never reaches for the
// repair list — a nil client must disable the stage rather than panic in it.
func TestSweepWithoutADocumentServiceSkipsTheRepair(t *testing.T) {
	s := &recordingStore{}
	task := &retentionTask{cfg: RetentionConfig{
		Store: s, Audit: audit.New(nil, nil, nil, "", zap.NewNop()),
		Batch: 10, Grace: time.Hour, Logger: zap.NewNop(),
	}}

	task.runOnce(context.Background())
	qt.Assert(t, qt.Equals(len(s.calls()), 1))
}
