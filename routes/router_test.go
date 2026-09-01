package routes

import (
	"testing"

	api "github.com/signbyte/envelope"

	"azugo.io/azugo"
	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"
)

func testApp(t testing.TB) *azugo.TestApp {
	app := api.TestApp(t)

	err := Init(app)
	qt.Assert(t, qt.IsNil(err))

	return azugo.NewTestApp(app.App)
}

func TestHealthzOK(t *testing.T) {
	app := testApp(t)
	app.Start(t)
	defer app.Stop()

	resp, err := app.TestClient().Get("/healthz")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)
}

func TestReadyzOK(t *testing.T) {
	app := testApp(t)
	app.Start(t)
	defer app.Stop()

	resp, err := app.TestClient().Get("/readyz")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
	fasthttp.ReleaseResponse(resp)
}

// The envelope API requires authentication.
func TestEnvelopesRequireAuth(t *testing.T) {
	app := testApp(t)
	app.Start(t)
	defer app.Stop()

	resp, err := app.TestClient().Post("/api/v1/envelopes", []byte("{}"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusUnauthorized))
	fasthttp.ReleaseResponse(resp)
}

// An authenticated caller without the matching scope is refused (403).
func TestEnvelopesScopeForbidden(t *testing.T) {
	app := testApp(t)
	app.Start(t)
	defer app.Stop()

	tc := app.TestClient()
	// Holds only the read scope, but create needs write.
	resp, err := tc.Post("/api/v1/envelopes", []byte("{}"), tc.WithHeader("X-Test-Scopes", "envelopes:read"))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(resp.StatusCode(), fasthttp.StatusForbidden))
	fasthttp.ReleaseResponse(resp)
}
