package routes

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	api "github.com/signbyte/envelope"
	"github.com/signbyte/envelope/audit"
	"github.com/signbyte/envelope/clients"

	"azugo.io/azugo"
	"github.com/gmb-lib/go-gdpr-audit/gdpr"
	"github.com/gmb-lib/go-platform-kit/broker"
	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"
)

// captureAudit captures everything the recorder emits: GDPR access records (as the
// gdpr poster) and workflow lifecycle events (as the broker transport), so a test
// can assert the rows + events landed.
type captureAudit struct {
	mu      sync.Mutex
	records []*broker.Envelope // GDPR personal-data-access records
	events  []*broker.Envelope // workflow lifecycle events
}

// Post captures a GDPR access record (gdpr.Poster).
func (c *captureAudit) Post(_ context.Context, rec *broker.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)

	return nil
}

// Publish captures a lifecycle event (broker.Transport).
func (c *captureAudit) Publish(_ context.Context, _, _ string, payload []byte) error {
	var ev broker.Envelope
	if err := json.Unmarshal(payload, &ev); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, &ev)

	return nil
}

// recordTypes / eventTypes return the captured event types, under the lock.
func (c *captureAudit) recordTypes() []string { return c.types(c.records) }
func (c *captureAudit) eventTypes() []string  { return c.types(c.events) }

func (c *captureAudit) types(in []*broker.Envelope) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(in))
	for i, e := range in {
		out[i] = e.EventType
	}

	return out
}

// hasRecordSubject reports whether any captured access record names the subject.
func (c *captureAudit) hasRecordSubject(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		for _, s := range r.DataSubjects {
			if s == sub {
				return true
			}
		}
	}

	return false
}

// newCaptureAudit builds a recorder whose access records + lifecycle events are
// captured, exercising the real go-gdpr-audit client + broker publisher.
func newCaptureAudit(t testing.TB) (*captureAudit, *audit.Recorder) {
	t.Helper()
	cap := &captureAudit{}
	gc, err := gdpr.New(gdpr.Configuration{
		Endpoint: "http://access-audit", Audience: "svc:access-audit",
		Scope: "access-audit:write", Timeout: time.Second,
	}, gdpr.PosterFunc(cap.Post))
	qt.Assert(t, qt.IsNil(err))

	// The security emitter is nil here: these tests assert the GDPR and lifecycle
	// channels, and the only security event this service emits comes from the
	// background sweep, which has its own tests.
	return cap, audit.New(nil, gc, broker.NewPublisher(cap, "envelope"), "envelope.events", nil)
}

// appWithAudit builds a route test app with a document stub + the capturing recorder.
func appWithAudit(t testing.TB, doer clients.Doer, rec *audit.Recorder) *azugo.TestApp {
	app := api.TestApp(t)
	qt.Assert(t, qt.IsNil(Init(app)))
	app.SetDocuments(clients.NewDocuments(doer, "http://document", "svc:document"))
	app.SetAudit(rec)

	return azugo.NewTestApp(app.App)
}

func contains(types []string, want string) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}

	return false
}

func count(types []string, want string) int {
	n := 0
	for _, t := range types {
		if t == want {
			n++
		}
	}

	return n
}

// The full create → send → sign → complete path writes the GDPR access records on
// the PII-bearing transitions and publishes the lifecycle events, completed
// included once the last slot signs.
func TestAuditRecordsAndEventsLand(t *testing.T) {
	cap, rec := newCaptureAudit(t)
	app := appWithAudit(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")}, rec)
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	env := createEnvelope(t, tc, `{"title":"c","orderPolicy":"sequential","documents":["doc-1"],"slots":[{"orderIndex":1,"identityRef":"id-signer-1"},{"orderIndex":2}]}`)
	qt.Assert(t, qt.Equals(len(env.SlotIDs), 2))
	slot1, slot2 := env.SlotIDs[0], env.SlotIDs[1]

	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil, tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	sf := app.TestClient()
	signCallback(t, sf, env.ID, slot1, `{"signatureId":"sig-1"}`)
	signed := signCallback(t, sf, env.ID, slot2, `{"signatureId":"sig-2"}`)
	qt.Assert(t, qt.Equals(signed.EnvelopeStatus, "completed"))

	// GDPR access records landed on the PII transitions (create + send), naming the
	// owner and the named slot identity.
	rt := cap.recordTypes()
	qt.Assert(t, qt.IsTrue(count(rt, "envelope.access") >= 2))
	qt.Assert(t, qt.IsTrue(cap.hasRecordSubject("svc:test-client"))) // the owner
	qt.Assert(t, qt.IsTrue(cap.hasRecordSubject("id-signer-1")))     // the named signer

	// Lifecycle events landed, completed once the last slot signed.
	et := cap.eventTypes()
	qt.Assert(t, qt.IsTrue(contains(et, audit.EventCreated)))
	qt.Assert(t, qt.IsTrue(contains(et, audit.EventSent)))
	qt.Assert(t, qt.Equals(count(et, audit.EventSlotSigned), 2))
	qt.Assert(t, qt.Equals(count(et, audit.EventCompleted), 1))
}

// A decline writes an access record + a declined event; a cancel publishes a
// cancelled event (no new personal data is processed by a cancel).
func TestAuditDeclineAndCancelEvents(t *testing.T) {
	cap, rec := newCaptureAudit(t)
	app := appWithAudit(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")}, rec)
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	// Decline path.
	env := createEnvelope(t, tc, `{"documents":["doc-1"],"slots":[{"orderIndex":1}]}`)
	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil, tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	fasthttp.ReleaseResponse(resp)

	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/slots/"+env.SlotIDs[0]+"/decline", nil, tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	// Cancel path (a fresh envelope).
	env2 := createEnvelope(t, tc, `{"documents":["doc-1"],"slots":[{"orderIndex":1}]}`)
	resp, err = tc.Post("/api/v1/envelopes/"+env2.ID+"/cancel", nil, tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	et := cap.eventTypes()
	qt.Assert(t, qt.IsTrue(contains(et, audit.EventDeclined)))
	qt.Assert(t, qt.IsTrue(contains(et, audit.EventCancelled)))
}
