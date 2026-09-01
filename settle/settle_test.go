package settle

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-quicktest/qt"
	"go.uber.org/zap"

	"github.com/signbyte/envelope/clients"
)

// perDocDocs answers a scripted retention per document and records the lifts.
type perDocDocs struct {
	answers  map[string]answer
	lifted   []string
	freezErr error
}

type answer struct {
	until time.Time
	live  int
	err   error
}

func (d *perDocDocs) SetResultFreeze(_ context.Context, id string, frozen bool) error {
	if !frozen {
		d.lifted = append(d.lifted, id)
	}

	return d.freezErr
}

func (d *perDocDocs) ChainRetention(_ context.Context, id string) (time.Time, int, error) {
	a := d.answers[id]

	return a.until, a.live, a.err
}

// recorder captures what was pinned, or that nothing was.
type recorder struct {
	called bool
	id     string
	until  time.Time
	err    error
}

func (r *recorder) SetRetentionUntil(_ context.Context, id string, until time.Time) error {
	r.called, r.id, r.until = true, id, until

	return r.err
}

// The envelope must outlive the LONGEST-lived of its documents. Taking any other one
// would drop the tracking page while a document it tracks is still downloadable.
func TestTerminalPinsTheLatestHorizonAcrossDocuments(t *testing.T) {
	early := time.Now().UTC().Add(24 * time.Hour)
	late := time.Now().UTC().Add(240 * time.Hour)
	docs := &perDocDocs{answers: map[string]answer{
		"a": {until: early, live: 1},
		"b": {until: late, live: 1},
	}}
	rec := &recorder{}

	err := Terminal(context.Background(), docs, rec, zap.NewNop(), "env-1", []string{"a", "b"})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsTrue(rec.until.Equal(late)))
	qt.Assert(t, qt.Equals(rec.id, "env-1"))
	qt.Assert(t, qt.DeepEquals(docs.lifted, []string{"a", "b"}))
}

// A chain holding nothing has no download left to outlive, so it puts no floor under
// the envelope — and an envelope with no live documents at all is settled at now,
// rather than waiting forever for a horizon that will never arrive.
func TestTerminalSettlesAtNowWhenNothingIsStored(t *testing.T) {
	docs := &perDocDocs{answers: map[string]answer{
		"a": {until: time.Now().UTC().Add(240 * time.Hour), live: 0},
	}}
	rec := &recorder{}

	before := time.Now().UTC()
	err := Terminal(context.Background(), docs, rec, zap.NewNop(), "env-1", []string{"a"})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsTrue(rec.called))
	qt.Assert(t, qt.IsFalse(rec.until.Before(before)))
	qt.Assert(t, qt.IsFalse(rec.until.After(time.Now().UTC())))
}

// A chain the document service no longer knows is an answer, not a failure: nothing
// is readable, so nothing is owed. Treating it as a failure would strand the envelope
// on the repair list forever, retried every sweep for a document that is never
// coming back.
func TestTerminalTreatsAMissingChainAsNothingToOutlive(t *testing.T) {
	docs := &perDocDocs{answers: map[string]answer{
		"gone": {err: &clients.HTTPError{Service: "document", StatusCode: http.StatusNotFound}},
	}}
	rec := &recorder{}

	err := Terminal(context.Background(), docs, rec, zap.NewNop(), "env-1", []string{"gone"})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsTrue(rec.called))
}

// Any other failure to read is NOT an answer. Nothing is pinned, and the caller is
// told — because a horizon nobody read is how a tracking page disappears early.
func TestTerminalRefusesToPinAHorizonItCouldNotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"transport", errors.New("connection refused")},
		{"server", &clients.HTTPError{Service: "document", StatusCode: http.StatusInternalServerError}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs := &perDocDocs{answers: map[string]answer{"a": {err: tc.err}}}
			rec := &recorder{}

			err := Terminal(context.Background(), docs, rec, zap.NewNop(), "env-1", []string{"a"})
			qt.Assert(t, qt.IsNotNil(err))
			qt.Assert(t, qt.IsFalse(rec.called))
		})
	}
}

// The two obligations are independent: a freeze that cannot be lifted leaves the
// chain locked-but-visible, and must not also cost the envelope its horizon — that
// would turn one recoverable failure into two.
func TestTerminalRecordsTheHorizonEvenWhenTheFreezeWillNotLift(t *testing.T) {
	until := time.Now().UTC().Add(240 * time.Hour)
	docs := &perDocDocs{
		answers:  map[string]answer{"a": {until: until, live: 1}},
		freezErr: errors.New("document service refused the lift"),
	}
	rec := &recorder{}

	err := Terminal(context.Background(), docs, rec, zap.NewNop(), "env-1", []string{"a"})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsTrue(rec.until.Equal(until)))
}
