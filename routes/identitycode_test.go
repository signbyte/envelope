package routes

import (
	"encoding/json"
	"strings"
	"testing"

	"azugo.io/azugo"
	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"
)

// inviteCoSigner creates an envelope whose second slot names identityRef, sends
// it, and returns the envelope and that slot. It stops at the create when the
// reference is refused, which is what the refusal tests assert instead.
func inviteCoSigner(t *testing.T, app *azugo.TestApp, identityRef string) (createEnvelopeResponse, string) {
	t.Helper()

	tc := app.TestClient()
	resp, err := tc.Post("/api/v1/envelopes",
		[]byte(`{"title":"contract","documents":["doc-1"],"slots":[{"orderIndex":1},{"orderIndex":2,"identityRef":"`+identityRef+`"}]}`),
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

	qt.Assert(t, qt.Equals(len(env.SlotIDs), 2))

	return env, env.SlotIDs[1]
}

// readsEnvelopeAs reports whether a caller presenting this identity code may read
// the envelope — which, for anyone who is not the owner, is exactly whether they
// match a slot.
func readsEnvelopeAs(t *testing.T, app *azugo.TestApp, envelopeID, serial string) bool {
	t.Helper()

	tc := app.TestClient()
	resp, err := tc.Get("/api/v1/envelopes/"+envelopeID,
		tc.WithHeader("X-Test-Scopes", "envelopes:read"), tc.WithHeader("X-Test-Sub", "user-b"),
		tc.WithHeader("X-Test-Serial", serial))
	qt.Assert(t, qt.IsNil(err))
	status := resp.StatusCode()
	fasthttp.ReleaseResponse(resp)

	return status == fasthttp.StatusOK
}

// The whole point of the work, at this service's own edge: a person invited under
// one spelling of their identity code is matched when they arrive under another.
// Both are reduced to the one stored spelling — the invitation on the way in, the
// caller's claim on the way back.
func TestCoSignerMatchesWhicheverSpellingEachSideUses(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "owner-a", "hash-1")})
	app.Start(t)
	defer app.Stop()

	written, stored := testIDCodeLVAsWritten(1), testIDCodeLV(1)
	qt.Assert(t, qt.Not(qt.Equals(written, stored)))

	// Invited the way a person writes their code.
	env, _ := inviteCoSigner(t, app, written)

	// Arrives the way the platform stores it, and the way a card certificate
	// writes it. Both reach the same slot.
	qt.Check(t, qt.IsTrue(readsEnvelopeAs(t, app, env.ID, stored)))
	qt.Check(t, qt.IsTrue(readsEnvelopeAs(t, app, env.ID, written)))
}

// What is STORED is the canonical spelling, whichever one was sent — so the match
// above is plain equality and not a looser comparison hiding somewhere.
func TestInvitedIdentityIsStoredCanonical(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "owner-a", "hash-1")})
	app.Start(t)
	defer app.Stop()

	env, _ := inviteCoSigner(t, app, testIDCodeLVAsWritten(2))

	tc := app.TestClient()
	resp, err := tc.Get("/api/v1/envelopes/"+env.ID,
		tc.WithHeader("X-Test-Scopes", "envelopes:read"), tc.WithHeader("X-Test-Sub", "owner-a"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	var view envelopeView
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &view)))
	fasthttp.ReleaseResponse(resp)

	qt.Assert(t, qt.Equals(len(view.Slots), 2))
	qt.Check(t, qt.Equals(view.Slots[1].IdentityRef, testIDCodeLV(2)))
}

// The same digits in another country are another person. A Lithuanian code
// matches a Latvian invitation nowhere — the country is part of the key, which is
// what makes cross-border co-signing safe to offer at all.
func TestAForeignCodeWithTheSameDigitsMatchesNothing(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "owner-a", "hash-1")})
	app.Start(t)
	defer app.Stop()

	env, _ := inviteCoSigner(t, app, testIDCodeLV(3))

	foreign := "PNOLT-" + strings.Repeat("3", 11)
	qt.Check(t, qt.IsFalse(readsEnvelopeAs(t, app, env.ID, foreign)))
}

// A bare national code is REFUSED, naming the field. This service is nowhere near
// the person and will not guess their country; the systems that are — the screen
// they type on, the register that sent them — supply it. The refusal repeats no
// identity code back: it is personal data.
func TestABareNationalCodeIsRefusedByName(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "owner-a", "hash-1")})
	app.Start(t)
	defer app.Stop()

	bare := strings.Repeat("4", 11)
	tc := app.TestClient()
	resp, err := tc.Post("/api/v1/envelopes",
		[]byte(`{"title":"contract","documents":["doc-1"],"slots":[{"orderIndex":1,"identityRef":"`+bare+`"}]}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"), tc.WithHeader("X-Test-Sub", "owner-a"),
		tc.WithHeader("Authorization", authToken))
	qt.Assert(t, qt.IsNil(err))
	body := string(resp.Body())
	qt.Check(t, qt.Equals(resp.StatusCode(), fasthttp.StatusUnprocessableEntity))
	fasthttp.ReleaseResponse(resp)

	qt.Check(t, qt.IsTrue(strings.Contains(body, "slots[0].identityRef")))
	qt.Check(t, qt.IsFalse(strings.Contains(body, bare)))
}

// The add-slot endpoint is the same door and applies the same rule — a rule
// enforced on one of two write paths is not enforced.
func TestAddSlotAppliesTheSameRule(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")})
	app.Start(t)
	defer app.Stop()

	tc := app.TestClient()
	env := createEnvelope(t, tc, `{"title":"contract","documents":["doc-1"]}`)

	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/slots",
		[]byte(`{"orderIndex":1,"identityRef":"`+strings.Repeat("5", 11)+`"}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(resp.StatusCode(), fasthttp.StatusUnprocessableEntity))
	fasthttp.ReleaseResponse(resp)

	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/slots",
		[]byte(`{"orderIndex":1,"identityRef":"`+testIDCodeLVAsWritten(5)+`"}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusCreated))
	fasthttp.ReleaseResponse(resp)

	resp, err = tc.Get("/api/v1/envelopes/"+env.ID,
		tc.WithHeader("X-Test-Scopes", "envelopes:read"), tc.WithHeader("X-Test-Sub", "svc:test-client"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	var view envelopeView
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &view)))
	fasthttp.ReleaseResponse(resp)

	qt.Assert(t, qt.Equals(len(view.Slots), 1))
	qt.Check(t, qt.Equals(view.Slots[0].IdentityRef, testIDCodeLV(5)))
}
