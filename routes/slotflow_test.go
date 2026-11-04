package routes

import (
	"encoding/json"
	"testing"

	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"
)

// A slot may name the legacyEid flow (the eID card through the provider's own
// browser plugin) like any other signing flow, and the envelope keeps it; a name
// that is not a signing flow is still refused.
func TestSlotTakesTheLegacyEIDFlow(t *testing.T) {
	app := appWithDocs(t, stubDoer{body: docMeta("doc-1", "svc:test-client", "hash-1")})
	app.Start(t)
	defer app.Stop()

	tc := app.TestClient()
	env := createEnvelope(t, tc, `{"title":"contract","documents":["doc-1"]}`)

	resp, err := tc.Post("/api/v1/envelopes/"+env.ID+"/slots",
		[]byte(`{"orderIndex":1,"flow":"legacyEID"}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Not(qt.Equals(resp.StatusCode(), fasthttp.StatusCreated)), qt.Commentf("%s", resp.Body()))
	fasthttp.ReleaseResponse(resp)

	resp, err = tc.Post("/api/v1/envelopes/"+env.ID+"/slots",
		[]byte(`{"orderIndex":1,"flow":"legacyEid"}`),
		tc.WithHeader("X-Test-Scopes", "envelopes:write"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusCreated), qt.Commentf("%s", resp.Body()))
	fasthttp.ReleaseResponse(resp)

	resp, err = tc.Get("/api/v1/envelopes/"+env.ID,
		tc.WithHeader("X-Test-Scopes", "envelopes:read"), tc.WithHeader("X-Test-Sub", "svc:test-client"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	var view envelopeView
	qt.Assert(t, qt.IsNil(json.Unmarshal(resp.Body(), &view)))
	fasthttp.ReleaseResponse(resp)

	qt.Assert(t, qt.Equals(len(view.Slots), 1))
	qt.Check(t, qt.Equals(view.Slots[0].Flow, "legacyEid"))
}
