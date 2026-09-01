package audit

import (
	"testing"
	"time"

	"github.com/go-quicktest/qt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/gmb-lib/go-platform-kit/broker"
	"github.com/gmb-lib/go-sec-events/secevents"
)

// observedRecorder returns a Recorder wired to a real security emitter and a logger
// whose lines can be read back — the security event IS a log line, so the log is
// where the assertion belongs.
func observedRecorder() (*Recorder, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.InfoLevel)

	return New(secevents.NewEmitter(secevents.NewLogSinkFor(zap.New(core))), nil, nil, "", zap.New(core)), logs
}

// attrs pulls the attribute map off the one recorded security event.
func attrs(t *testing.T, logs *observer.ObservedLogs) map[string]any {
	t.Helper()

	entries := logs.FilterMessage("security_event").All()
	qt.Assert(t, qt.Equals(len(entries), 1))

	for _, f := range entries[0].Context {
		if f.Key == "attributes" {
			m, ok := f.Interface.(map[string]any)
			qt.Assert(t, qt.IsTrue(ok))

			return m
		}
	}
	t.Fatal("the security event carries no attributes field")

	return nil
}

// A sweep that erased something records what it erased and under which windows —
// this is the accountability record, so every number a reader would need is on it.
func TestRetentionSweptRecordsCountsAndWindows(t *testing.T) {
	r, logs := observedRecorder()

	r.RetentionSwept(2, 5, 3, 0, 216*time.Hour, 720*time.Hour)

	entries := logs.FilterMessage("security_event").All()
	qt.Assert(t, qt.Equals(len(entries), 1))

	a := attrs(t, logs)
	qt.Assert(t, qt.Equals(a["expired"], 2))
	qt.Assert(t, qt.Equals(a["terminal_deleted"], 5))
	qt.Assert(t, qt.Equals(a["drafts_deleted"], 3))
	qt.Assert(t, qt.Equals(a["grace"], "216h0m0s"))
	qt.Assert(t, qt.Equals(a["draft_ttl"], "720h0m0s"))
	qt.Assert(t, qt.Equals(a[secevents.AttrSeverity], any(string(secevents.SeverityInfo))))

	var eventType string
	for _, f := range entries[0].Context {
		if f.Key == "event_type" {
			eventType = f.String
		}
	}
	qt.Assert(t, qt.Equals(eventType, EventRetentionSwept))
}

// `erased` is the count a rule watching for personal-data erasure reads, so it must
// be the two DELETE stages and not the expiry — expiring an envelope changes its
// status and erases nothing.
func TestRetentionSweptErasedCountsOnlyDeletions(t *testing.T) {
	r, logs := observedRecorder()

	r.RetentionSwept(7, 5, 3, 0, time.Hour, time.Hour)

	qt.Assert(t, qt.Equals(attrs(t, logs)["erased"], 8))
}

// A sweep that expired an envelope but deleted nothing still records itself: the
// transition is a state change worth seeing, and `erased` says plainly that nothing
// was removed.
func TestRetentionSweptRecordsAnExpiryOnlySweep(t *testing.T) {
	r, logs := observedRecorder()

	r.RetentionSwept(4, 0, 0, 0, time.Hour, time.Hour)

	qt.Assert(t, qt.Equals(attrs(t, logs)["erased"], 0))
}

// A sweep that found nothing writes nothing. The sweep runs every hour on most
// deployments and finds nothing most of the time; an hourly "erased 0" would bury
// the events that matter in the stream meant to surface them.
func TestRetentionSweptSaysNothingWhenNothingHappened(t *testing.T) {
	r, logs := observedRecorder()

	r.RetentionSwept(0, 0, 0, 0, time.Hour, time.Hour)

	qt.Assert(t, qt.Equals(logs.FilterMessage("security_event").Len(), 0))
}

// The event never names an envelope or a person. Recording who was erased would put
// back, in the security stream, exactly what the erasure removed — so the assertion
// is on the whole attribute set, not on a list of keys someone remembered to check.
func TestRetentionSweptNamesNoIdentities(t *testing.T) {
	r, logs := observedRecorder()

	r.RetentionSwept(1, 1, 1, 0, time.Hour, time.Hour)

	allowed := map[string]bool{
		"expired": true, "terminal_deleted": true, "drafts_deleted": true,
		"erased": true, "awaiting_horizon": true, "grace": true, "draft_ttl": true,
		secevents.AttrSeverity: true,
	}
	for k := range attrs(t, logs) {
		qt.Assert(t, qt.IsTrue(allowed[k]))
	}

	entries := logs.FilterMessage("security_event").All()
	for _, f := range entries[0].Context {
		qt.Assert(t, qt.IsFalse(f.Key == "actor_id"))
		qt.Assert(t, qt.IsFalse(f.Key == "data_subjects"))
		qt.Assert(t, qt.IsFalse(f.Key == "resource_id"))
	}
}

// A sweep that only deleted — nothing lapsed this hour — is the erasure case, and
// it must record itself just as loudly as a mixed one. It reaches the SIEM marked
// as a deletion, so a rule grouping by operation sees the act for what it is
// rather than having to infer it from the event type.
func TestRetentionSweptRecordsADeletionOnlySweep(t *testing.T) {
	r, logs := observedRecorder()

	r.RetentionSwept(0, 1, 2, 0, time.Hour, time.Hour)

	a := attrs(t, logs)
	qt.Assert(t, qt.Equals(a["expired"], 0))
	qt.Assert(t, qt.Equals(a["erased"], 3))

	var op string
	for _, f := range logs.FilterMessage("security_event").All()[0].Context {
		if f.Key == "operation" {
			op = f.String
		}
	}
	qt.Assert(t, qt.Equals(op, string(broker.OpDelete)))
}

// An unconfigured recorder — and a nil one — must not take the sweep down with it.
// The sweep's job is to erase; failing it because telemetry is unwired would be the
// wrong trade.
func TestRetentionSweptIsSafeWithoutAnEmitter(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	New(nil, nil, nil, "", zap.New(core)).RetentionSwept(1, 1, 1, 0, time.Hour, time.Hour)
	qt.Assert(t, qt.Equals(logs.FilterMessage("security_event").Len(), 0))

	var nilRecorder *Recorder
	nilRecorder.RetentionSwept(1, 1, 1, 0, time.Hour, time.Hour)
}
