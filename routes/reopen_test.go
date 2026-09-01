package routes

import (
	"encoding/json"
	"strings"
	"testing"

	"azugo.io/azugo"

	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"
)

// A completed envelope is the workflow home of its chain, so a further signature joins
// IT rather than a second envelope being minted over the same container. Reopen takes
// it back to draft with its signed slots intact, a new slot is added beside them, and
// the send that closes the round is what re-grants chain access — reopen deliberately
// does not open signing by itself, because add_slot grants nothing.
func TestReopenCompletedEnvelopeTakesASecondRound(t *testing.T) {
	coSerial := testIDCodeLV(1)
	doer := &recordingDoer{meta: docMeta("doc-1", "svc:test-client", "hash-1")}
	app := appWithDocs(t, doer)
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	env := createEnvelope(t, tc, `{"title":"first round","documents":["doc-1"],"slots":[{"orderIndex":1}]}`)
	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil,
		tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	// The first round freezes the chain, as it must: nothing may be downloaded while
	// the result is half-signed.
	qt.Assert(t, qt.Equals(countFreezes(doer.freezes, true), 1))

	sf := app.TestClient()
	signCallback(t, sf, env.ID, env.SlotIDs[0], `{"signatureId":"sig-1"}`)

	// Reopen: back to draft, same envelope, and the version moves so a concurrent act
	// cannot be lost.
	reopened := reopenEnvelopeForTest(t, tc, env.ID)
	qt.Assert(t, qt.Equals(reopened.Status, "draft"))
	qt.Check(t, qt.Equals(reopened.ID, env.ID))

	// The previous round's signed slot is still there — it IS the record of that round
	// — and the new slot is added beside it, which the draft-only add_slot guard now
	// permits on its own terms rather than being weakened.
	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/slots",
		[]byte(`{"orderIndex":2,"identityRef":"`+coSerial+`"}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"), tc.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusCreated))
	fasthttp.ReleaseResponse(resp)

	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil,
		tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)

	// The re-send re-granted access, which is the whole reason a round must be closed
	// by a send: the new signer can reach the document.
	qt.Assert(t, qt.IsTrue(len(doer.grants) >= 1))
	qt.Check(t, qt.IsTrue(strings.Contains(strings.Join(doer.grants, "|"), coSerial)))

	// And the chain was NOT frozen again: still exactly ONE freeze across both rounds,
	// the first round's. A re-freeze would take an already-released signed result back
	// from the people who could download it, because someone else is adding a
	// signature. Counted by content, because this recorder holds the LIFTS too (the
	// first round's completion lifted the freeze, which is why the raw call count is
	// not the assertion).
	qt.Assert(t, qt.Equals(countFreezes(doer.freezes, true), 1))
	qt.Check(t, qt.Equals(countFreezes(doer.freezes, false), 1))

	// One envelope, two slots, the first still signed.
	resp, err = tc.Get("/api/v1/envelopes/"+env.ID, tc.WithHeader("X-Test-Scopes", "envelopes:read"))
	qt.Assert(t, qt.IsNil(err))
	var view envelopeView
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &view)))
	fasthttp.ReleaseResponse(resp)
	qt.Check(t, qt.Equals(view.Envelope.Status, "sent"))
	qt.Assert(t, qt.Equals(len(view.Slots), 2))
	signedSlots := 0
	for _, s := range view.Slots {
		if s.Status == "signed" {
			signedSlots++
		}
	}
	qt.Check(t, qt.Equals(signedSlots, 1))
}

// Only a completed envelope reopens. Declined, cancelled and expired are closed for a
// reason that is not "another signature is coming", and a draft has nothing to reopen —
// each refusal is a 409 rather than a silent no-op, so a caller cannot mistake a
// rejected reopen for a fresh round.
func TestReopenRefusesEverythingButCompleted(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")})
	app.Start(t)
	defer app.Stop()
	tc := app.TestClient()

	// A draft: never sent, nothing to reopen.
	env := createEnvelope(t, tc, `{"title":"draft","documents":["doc-1"],"slots":[{"orderIndex":1}]}`)
	assertReopenRefused(t, tc, env.ID)

	// Sent, not yet signed: the round is live, so reopening is meaningless.
	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/send", nil,
		tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	fasthttp.ReleaseResponse(resp)
	assertReopenRefused(t, tc, env.ID)

	// Cancelled: terminal, but closed on purpose.
	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/cancel", nil,
		tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)
	assertReopenRefused(t, tc, env.ID)
}

// Reopen is the owner's act. A caller who is not the owner must not be able to move
// someone else's envelope out of a terminal state — and the answer is a not-found
// rather than a forbidden, so the route does not confirm the envelope exists.
func TestReopenIsOwnerOnly(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")})
	app.Start(t)
	defer app.Stop()

	tcA := app.TestClient()
	env := createEnvelope(t, tcA, `{"title":"mine","documents":["doc-1"],"slots":[{"orderIndex":1}]}`)
	resp, err := tcA.Post("/api/v1/envelopes/"+env.ID+"/send", nil,
		tcA.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	fasthttp.ReleaseResponse(resp)
	sf := app.TestClient()
	signCallback(t, sf, env.ID, env.SlotIDs[0], `{"signatureId":"sig-1"}`)

	stranger := app.TestClient()
	resp, err = stranger.Post("/api/v1/envelopes/"+env.ID+"/reopen", nil,
		stranger.WithHeader("X-Test-Scopes", "envelopes:transition"),
		stranger.WithHeader("X-Test-Sub", "someone-else"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(resp.StatusCode(), fasthttp.StatusNotFound))
	fasthttp.ReleaseResponse(resp)
}

// reopenEnvelopeForTest posts the reopen and returns the decoded transition.
func reopenEnvelopeForTest(t *testing.T, tc *azugo.TestClient, envID string) transitionResponse {
	t.Helper()
	resp, err := tc.Post("/api/v1/envelopes/"+envID+"/reopen", nil,
		tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	var out transitionResponse
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &out)))
	fasthttp.ReleaseResponse(resp)

	return out
}

// assertReopenRefused requires the reopen to be refused as a state conflict.
func assertReopenRefused(t *testing.T, tc *azugo.TestClient, envID string) {
	t.Helper()
	resp, err := tc.Post("/api/v1/envelopes/"+envID+"/reopen", nil,
		tc.WithHeader("X-Test-Scopes", "envelopes:transition"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(resp.StatusCode(), fasthttp.StatusConflict))
	qt.Check(t, qt.IsTrue(strings.Contains(string(resp.Body()), "err:envelope:invalidState")))
	fasthttp.ReleaseResponse(resp)
}

// countFreezes counts the result-freeze calls of one polarity. The recorder keeps
// freezes and lifts in one list, so a bare length says nothing about either.
func countFreezes(calls []string, frozen bool) int {
	want := `"frozen":false`
	if frozen {
		want = `"frozen":true`
	}
	n := 0
	for _, c := range calls {
		if strings.Contains(c, want) {
			n++
		}
	}

	return n
}
