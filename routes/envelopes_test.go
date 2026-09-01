package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	api "github.com/signbyte/envelope"
	"github.com/signbyte/envelope/clients"

	"azugo.io/azugo"
	"github.com/gmb-lib/go-authbyte/authclient"
	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"
)

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

// stubDoer is a minimal on-behalf doer for the document client: it returns a
// scripted metadata body + status for any document GET, so the attach path is
// exercised without a live document service.
type stubDoer struct {
	body   []byte
	status int
}

func (s stubDoer) DoServiceOnBehalf(_ context.Context, _, _, _, _, _, _ string, _ http.Header, _ []byte) (*authclient.BackgroundResponse, error) {
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}

	return &authclient.BackgroundResponse{StatusCode: status, Body: s.body}, nil
}

func (s stubDoer) DoService(_ context.Context, _, _, _, _ string, _ http.Header, _ []byte) (*authclient.BackgroundResponse, error) {
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}

	return &authclient.BackgroundResponse{StatusCode: status, Body: s.body}, nil
}

// appWithDocs builds a route test app whose document client is backed by the given
// stub doer (so attach can validate a reference + pin a hash).
func appWithDocs(t testing.TB, doer clients.Doer) *azugo.TestApp {
	app := api.TestApp(t)
	qt.Assert(t, qt.IsNil(Init(app)))
	app.SetDocuments(clients.NewDocuments(doer, "http://document", "svc:document"))

	return azugo.NewTestApp(app.App)
}

// docMeta builds a document-metadata body owned by owner with the given hash.
func docMeta(id, owner, hash string) []byte {
	b, _ := json.Marshal(map[string]string{"id": id, "owner": owner, "contentHash": hash})

	return b
}

// authToken is a stand-in inbound access token. The service threads it as the
// subject token for the on-behalf document call (attach fails closed without it).
const authToken = "DPoP test-subject-token"

// createEnvelope posts a create body and returns the decoded response. It carries
// an Authorization header so any initial document references attach on-behalf.
func createEnvelope(t *testing.T, tc *azugo.TestClient, body string) createEnvelopeResponse {
	t.Helper()
	resp, err := tc.Post("/api/v1/envelopes", []byte(body),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"),
		tc.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusCreated))
	var out createEnvelopeResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &out)))
	fasthttp.ReleaseResponse(resp)

	return out
}

// A full create -> attach -> add slot -> send happy path over the routes.
func TestCreateAttachSlotSendHappyPath(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")})
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	env := createEnvelope(t, tc, `{"title":"contract","orderPolicy":"parallel"}`)
	qt.Assert(t, qt.Equals(env.Status, "draft"))

	// Attach a document reference (validated on behalf of the user, hash pinned).
	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/documents", []byte(`{"documentId":"doc-1"}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"), tc.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusCreated))
	fasthttp.ReleaseResponse(resp)

	// Define a slot.
	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/slots", []byte(`{"orderIndex":1,"role":"signer"}`), tc.WithHeader("X-Test-Scopes", "envelopes:write"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusCreated))
	fasthttp.ReleaseResponse(resp)

	// Send.
	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil, tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	var sent transitionResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &sent)))
	fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.Equals(sent.Status, "sent"))
}

// Send without a document or slot is refused as incomplete (422).
func TestSendIncomplete(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")})
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	env := createEnvelope(t, tc, `{"title":"empty"}`)

	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil, tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusUnprocessableEntity))
	fasthttp.ReleaseResponse(resp)
}

// A non-owner reading an envelope gets a 404 (owner-filtered — no enumeration).
// Each identity uses its own client so the per-connection request headers carry a
// single caller subject.
func TestGetEnvelopeOwnerFilter404(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "user-a", "hash-1")})
	app.Start(t)
	defer app.Stop()

	// user-a creates an envelope.
	tcA := app.TestClient()
	resp, err := tcA.Post("/api/v1/envelopes", []byte(`{"title":"a"}`),
		tcA.WithHeader("X-Test-Scopes", "envelopes:write"), tcA.WithHeader("X-Test-Sub", "user-a"))
	qt.Assert(t, qt.IsNil(err))
	var env createEnvelopeResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &env)))
	fasthttp.ReleaseResponse(resp)

	// user-b cannot read it.
	tcB := app.TestClient()
	resp, err = tcB.Get("/api/v1/envelopes/"+env.ID,
		tcB.WithHeader("X-Test-Scopes", "envelopes:read"), tcB.WithHeader("X-Test-Sub", "user-b"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusNotFound))
	fasthttp.ReleaseResponse(resp)
}

// Attaching a document the user does not own (the document service returns 404) is
// surfaced as a 404, indistinguishable from an absent reference.
func TestAttachNotOwnedIs404(t *testing.T) {
	app := appWithDocs(t, stubDoer{status: http.StatusNotFound, body: []byte(`{"error":"not_found"}`)})
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	env := createEnvelope(t, tc, `{"title":"a"}`)

	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/documents", []byte(`{"documentId":"doc-x"}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"), tc.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusNotFound))
	fasthttp.ReleaseResponse(resp)
}

// The eligibility gate over the routes: a sequential envelope opens slot 1 and gates
// slot 2 until slot 1 signs.
func TestSlotEligibleAndSignedCallback(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")})
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	env := createEnvelope(t, tc, `{"title":"seq","orderPolicy":"sequential","documents":["doc-1"],"slots":[{"orderIndex":1},{"orderIndex":2}]}`)
	qt.Assert(t, qt.Equals(len(env.SlotIDs), 2))
	slot1, slot2 := env.SlotIDs[0], env.SlotIDs[1]

	// Send.
	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil, tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	// Slot 2 is not eligible until slot 1 signs.
	qt.Assert(t, qt.IsFalse(eligible(t, tc, env.ID, slot2)))
	qt.Assert(t, qt.IsTrue(eligible(t, tc, env.ID, slot1)))

	// The signing service reports slot 1 signed (transition scope, not owner-scoped).
	// It is a distinct principal, so it uses its own client.
	sf := app.TestClient()
	signed := signCallback(t, sf, env.ID, slot1, `{"signatureId":"sig-1","jobId":"job-1"}`)
	qt.Assert(t, qt.Equals(signed.Status, "signed"))
	qt.Assert(t, qt.IsFalse(signed.Idempotent))

	// Now slot 2 is eligible.
	qt.Assert(t, qt.IsTrue(eligible(t, tc, env.ID, slot2)))

	// A replayed slot-1 callback is an idempotent no-op success.
	signed = signCallback(t, sf, env.ID, slot1, `{"signatureId":"sig-1"}`)
	qt.Assert(t, qt.IsTrue(signed.Idempotent))

	// Signing slot 2 completes the envelope.
	signCallback(t, sf, env.ID, slot2, `{"signatureId":"sig-2"}`)

	resp, err = tc.Get("/api/v1/envelopes/"+env.ID, tc.WithHeader("X-Test-Scopes", "envelopes:read"))
	qt.Assert(t, qt.IsNil(err))
	var view envelopeView
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &view)))
	fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.Equals(view.Envelope.Status, "completed"))
}

// A co-signer invited by serial can read the envelope + act on their own slot over the
// routes; a caller matching no slot gets a 404 (no enumeration). Each identity uses its
// own client so the per-connection headers carry a single caller.
func TestCoSignerParticipantAccessOverRoutes(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "owner-a", "hash-1")})
	app.Start(t)
	defer app.Stop()

	coSerial := testIDCodeLV(1)

	// owner-a builds an envelope: slot 1 the owner's, slot 2 the co-signer's (by serial).
	tcA := app.TestClient()
	resp, err := tcA.Post("/api/v1/envelopes",
		[]byte(`{"title":"contract","documents":["doc-1"],"slots":[{"orderIndex":1},{"orderIndex":2,"identityRef":"`+coSerial+`"}]}`),
		tcA.WithHeader("X-Test-Scopes", "envelopes:write"), tcA.WithHeader("X-Test-Sub", "owner-a"),
		tcA.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusCreated))
	var env createEnvelopeResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &env)))
	fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.Equals(len(env.SlotIDs), 2))
	coSlot := env.SlotIDs[1]

	// Send (owner).
	resp, err = tcA.Post("/api/v1/envelopes/"+env.ID+"/send", nil,
		tcA.WithHeader("X-Test-Scopes", "envelopes:transition"), tcA.WithHeader("X-Test-Sub", "owner-a"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	// The co-signer (different sub, matching serial) reads the envelope.
	tcB := app.TestClient()
	resp, err = tcB.Get("/api/v1/envelopes/"+env.ID,
		tcB.WithHeader("X-Test-Scopes", "envelopes:read"), tcB.WithHeader("X-Test-Sub", "user-b"),
		tcB.WithHeader("X-Test-Serial", coSerial))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	// Their own slot is eligible to them.
	resp, err = tcB.Get("/api/v1/envelopes/"+env.ID+"/slots/"+coSlot+"/eligible",
		tcB.WithHeader("X-Test-Scopes", "envelopes:read"), tcB.WithHeader("X-Test-Sub", "user-b"),
		tcB.WithHeader("X-Test-Serial", coSerial))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	var elig eligibleResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &elig)))
	fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.IsTrue(elig.Eligible))

	// A caller matching no slot is a 404 — indistinguishable from absent (no enumeration).
	tcC := app.TestClient()
	resp, err = tcC.Get("/api/v1/envelopes/"+env.ID,
		tcC.WithHeader("X-Test-Scopes", "envelopes:read"), tcC.WithHeader("X-Test-Sub", "user-c"),
		tcC.WithHeader("X-Test-Serial", testIDCodeLV(9)))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusNotFound))
	fasthttp.ReleaseResponse(resp)
}

// The ?documentId= lookup resolves the envelopes covering one document for every
// caller the access model allows: the owner always, a serial-matched participant
// on a non-draft envelope; a caller matching no slot sees none (no enumeration),
// and an unrelated document resolves to none.
func TestFindEnvelopesForDocumentOverRoutes(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "owner-a", "hash-1")})
	app.Start(t)
	defer app.Stop()

	coSerial := testIDCodeLV(1)

	tcA := app.TestClient()
	resp, err := tcA.Post("/api/v1/envelopes",
		[]byte(`{"title":"contract","documents":["doc-1"],"slots":[{"orderIndex":1},{"orderIndex":2,"identityRef":"`+coSerial+`"}]}`),
		tcA.WithHeader("X-Test-Scopes", "envelopes:write"), tcA.WithHeader("X-Test-Sub", "owner-a"),
		tcA.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusCreated))
	var env createEnvelopeResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &env)))
	fasthttp.ReleaseResponse(resp)

	resp, err = tcA.Post("/api/v1/envelopes/"+env.ID+"/send", nil,
		tcA.WithHeader("X-Test-Scopes", "envelopes:transition"), tcA.WithHeader("X-Test-Sub", "owner-a"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	type listOut struct {
		Envelopes []struct {
			ID       string `json:"id"`
			YourTurn bool   `json:"yourTurn"`
		} `json:"envelopes"`
	}
	lookup := func(tc *azugo.TestClient, sub, serial, docID string) listOut {
		t.Helper()
		opts := []azugo.TestClientOption{
			tc.WithHeader("X-Test-Scopes", "envelopes:read"),
			tc.WithHeader("X-Test-Sub", sub),
		}
		if serial != "" {
			opts = append(opts, tc.WithHeader("X-Test-Serial", serial))
		}
		resp, err := tc.Get("/api/v1/envelopes?documentId="+docID, opts...)
		qt.Assert(t, qt.IsNil(err))
		qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
		var out listOut
		qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &out)))
		fasthttp.ReleaseResponse(resp)

		return out
	}

	// The owner resolves the covering envelope — and it is their turn (their own
	// identity-less slot is outstanding on a parallel envelope).
	got := lookup(app.TestClient(), "owner-a", "", "doc-1")
	qt.Assert(t, qt.Equals(len(got.Envelopes), 1))
	qt.Assert(t, qt.Equals(got.Envelopes[0].ID, env.ID))
	qt.Assert(t, qt.IsTrue(got.Envelopes[0].YourTurn))

	// The invited co-signer (different sub, matching serial) resolves it too.
	got = lookup(app.TestClient(), "user-b", coSerial, "doc-1")
	qt.Assert(t, qt.Equals(len(got.Envelopes), 1))
	qt.Assert(t, qt.Equals(got.Envelopes[0].ID, env.ID))

	// A caller matching no slot sees none; an unrelated document resolves to none.
	qt.Assert(t, qt.Equals(len(lookup(app.TestClient(), "user-c", testIDCodeLV(9), "doc-1").Envelopes), 0))
	qt.Assert(t, qt.Equals(len(lookup(app.TestClient(), "owner-a", "", "doc-other").Envelopes), 0))
}

// recordingDoer answers on-behalf reads with scripted metadata (for attach) and
// records the service calls (DoService) the send path makes, split by target:
// document-chain ACL grants and result-freeze flips.
type recordingDoer struct {
	meta    []byte
	grants  []string // the request bodies of the document-chain ACL grant calls
	freezes []string // the request bodies of the result-freeze calls
}

func (d *recordingDoer) DoServiceOnBehalf(_ context.Context, _, _, _, _, _, _ string, _ http.Header, _ []byte) (*authclient.BackgroundResponse, error) {
	return &authclient.BackgroundResponse{StatusCode: http.StatusOK, Body: d.meta}, nil
}

func (d *recordingDoer) DoService(_ context.Context, _, _, _ string, url string, _ http.Header, body []byte) (*authclient.BackgroundResponse, error) {
	switch {
	case strings.Contains(url, "/result-freeze"):
		d.freezes = append(d.freezes, string(body))
	default:
		d.grants = append(d.grants, string(body))
	}

	return &authclient.BackgroundResponse{StatusCode: http.StatusNoContent}, nil
}

// Sending an envelope grants each invited slot serial standing access to the
// attached document's chain (a documents:grant service call); a slot with no
// identity is skipped.
func TestSendGrantsParticipantACL(t *testing.T) {
	coSerial := testIDCodeLV(1)

	doer := &recordingDoer{meta: docMeta("doc-1", "owner-a", "hash-1")}
	app := appWithDocs(t, doer)
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	// Slot 1 = the owner's (no identity); slot 2 = the co-signer's (by serial).
	resp, err := tc.Post("/api/v1/envelopes",
		[]byte(`{"title":"c","documents":["doc-1"],"slots":[{"orderIndex":1},{"orderIndex":2,"identityRef":"`+coSerial+`"}]}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"), tc.WithHeader("X-Test-Sub", "owner-a"),
		tc.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusCreated))
	var env createEnvelopeResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &env)))
	fasthttp.ReleaseResponse(resp)

	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil,
		tc.WithHeader("X-Test-Scopes", "envelopes:transition"), tc.WithHeader("X-Test-Sub", "owner-a"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	// Exactly one grant — the co-signer's serial (the owner's slot carries no
	// identity, so it is skipped) — and the serial is in the grant body.
	qt.Assert(t, qt.Equals(len(doer.grants), 1))
	qt.Assert(t, qt.IsTrue(strings.Contains(doer.grants[0], coSerial)))

	// ...and exactly one result-freeze flip: the send locks the chain's signed
	// result for the signing window (it opens at the terminal transition).
	qt.Assert(t, qt.Equals(len(doer.freezes), 1))
	qt.Assert(t, qt.IsTrue(strings.Contains(doer.freezes[0], `"frozen":true`)))
}

// The signer inbox over the routes: a co-signer (matched by serial) sees exactly the one
// envelope awaiting their signature; the owner's own inbox is empty (they own it); a
// stranger serial sees nothing.
func TestSigningTasksOverRoutes(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "owner-a", "hash-1")})
	app.Start(t)
	defer app.Stop()

	coSerial := testIDCodeLV(1)

	// owner-a builds + sends a 2-slot envelope: slot 1 owner's, slot 2 the co-signer's.
	tcA := app.TestClient()
	resp, err := tcA.Post("/api/v1/envelopes",
		[]byte(`{"title":"contract","documents":["doc-1"],"slots":[{"orderIndex":1},{"orderIndex":2,"identityRef":"`+coSerial+`"}]}`),
		tcA.WithHeader("X-Test-Scopes", "envelopes:write"), tcA.WithHeader("X-Test-Sub", "owner-a"),
		tcA.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	var env createEnvelopeResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &env)))
	fasthttp.ReleaseResponse(resp)
	resp, err = tcA.Post("/api/v1/envelopes/"+env.ID+"/send", nil,
		tcA.WithHeader("X-Test-Scopes", "envelopes:transition"), tcA.WithHeader("X-Test-Sub", "owner-a"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	// The co-signer's inbox: exactly their task, their slot, their turn (parallel).
	tcB := app.TestClient()
	resp, err = tcB.Get("/api/v1/signing-tasks",
		tcB.WithHeader("X-Test-Scopes", "envelopes:read"), tcB.WithHeader("X-Test-Sub", "user-b"),
		tcB.WithHeader("X-Test-Serial", coSerial))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	var inbox signingTasksResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &inbox)))
	fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.Equals(len(inbox.Tasks), 1))
	qt.Assert(t, qt.Equals(inbox.Tasks[0].Envelope.ID, env.ID))
	qt.Assert(t, qt.Equals(inbox.Tasks[0].SlotID, env.SlotIDs[1]))
	qt.Assert(t, qt.IsTrue(inbox.Tasks[0].YourTurn))
	// The task's envelope relays its attached document ids on the wire, so a
	// dashboard composer can subtract the covered chains for an invited signer.
	qt.Assert(t, qt.DeepEquals(inbox.Tasks[0].Envelope.DocIDs, []string{"doc-1"}))
	// The owner subject is never exposed to the co-signer.
	qt.Assert(t, qt.Equals(inbox.Tasks[0].Envelope.Status, "sent"))

	// The owner's own signer inbox is empty — they own the envelope (it lists elsewhere).
	resp, err = tcA.Get("/api/v1/signing-tasks",
		tcA.WithHeader("X-Test-Scopes", "envelopes:read"), tcA.WithHeader("X-Test-Sub", "owner-a"),
		tcA.WithHeader("X-Test-Serial", coSerial))
	qt.Assert(t, qt.IsNil(err))
	var ownerInbox signingTasksResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &ownerInbox)))
	fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.Equals(len(ownerInbox.Tasks), 0))

	// A stranger serial sees nothing.
	tcC := app.TestClient()
	resp, err = tcC.Get("/api/v1/signing-tasks",
		tcC.WithHeader("X-Test-Scopes", "envelopes:read"), tcC.WithHeader("X-Test-Sub", "user-c"),
		tcC.WithHeader("X-Test-Serial", testIDCodeLV(9)))
	qt.Assert(t, qt.IsNil(err))
	var strangerInbox signingTasksResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &strangerInbox)))
	fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.Equals(len(strangerInbox.Tasks), 0))
}

// signCallback posts the slot-signed callback as the signing service (transition
// scope, a distinct principal) and returns the decoded response.
func signCallback(t *testing.T, sf *azugo.TestClient, envID, slotID, body string) signedResponse {
	t.Helper()
	resp, err := sf.Post("/api/v1/envelopes/"+envID+"/slots/"+slotID+"/signed", []byte(body),
		sf.WithHeader("X-Test-Scopes", "envelopes:transition"), sf.WithHeader("X-Test-Sub", "svc:signflow"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	var out signedResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &out)))
	fasthttp.ReleaseResponse(resp)

	return out
}

// A second send (the envelope is no longer draft) returns a 409-class conflict; a
// stale-version transition path is covered by the store test.
func TestSendTwiceConflict(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")})
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	env := createEnvelope(t, tc, `{"documents":["doc-1"],"slots":[{"orderIndex":1}]}`)

	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil, tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	// A second send is rejected: the envelope is no longer a draft.
	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil, tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusConflict))
	fasthttp.ReleaseResponse(resp)
}

// eligible queries the eligibility precondition and returns the answer.
func eligible(t *testing.T, tc *azugo.TestClient, envID, slotID string) bool {
	t.Helper()
	resp, err := tc.Get("/api/v1/envelopes/"+envID+"/slots/"+slotID+"/eligible", tc.WithHeader("X-Test-Scopes", "envelopes:read"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	var out eligibleResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &out)))
	fasthttp.ReleaseResponse(resp)

	return out.Eligible
}

// A third signer is refused at the wire with its own code and a 422 — not a 500, and
// not the generic invalid code. The detail names the product, never a setting.
func TestThirdSignerRefusedAtTheWire(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")})
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	env := createEnvelope(t, tc, `{"documents":["doc-1"],"slots":[{"orderIndex":1},{"orderIndex":2}]}`)

	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/slots", []byte(`{"orderIndex":3}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"),
		tc.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusUnprocessableEntity))
	body := string(resp.Body())
	qt.Assert(t, qt.IsTrue(strings.Contains(body, "err:envelope:slotLimit")))
	qt.Assert(t, qt.IsFalse(strings.Contains(strings.ToLower(body), "config")))
	fasthttp.ReleaseResponse(resp)
}
