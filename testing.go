package envelope

import (
	"testing"

	"azugo.io/azugo"
	"azugo.io/azugo/token"
	"azugo.io/azugo/user"
	"github.com/go-quicktest/qt"
)

// TestApp builds an App for tests: no document wiring, the in-memory store, and a
// stub auth middleware driven by the X-Test-Scopes request header (production
// always uses the go-authbyte DPoP middleware).
func TestApp(tb testing.TB) *App {
	tb.Helper()

	tb.Setenv("METRICS_ENABLED", "false")
	tb.Setenv("SERVICE_NAME", "envelope")
	tb.Setenv("ENVIRONMENT", "development")
	tb.Setenv("AUTH_ISSUER_URL", "http://localhost:8080")
	tb.Setenv("SERVICE_AUDIENCE", "svc:envelope")

	app, err := New(nil, "0.0.0-test")
	qt.Assert(tb, qt.IsNil(err))

	app.SetAuthMiddleware(TestAuthMiddleware())

	return app
}

// TestAuthMiddleware authenticates requests from the X-Test-Scopes header
// (comma-separated scopes) and uses the optional X-Test-Sub header as the caller
// identity (default "svc:test-client"). The optional X-Test-Serial header sets the
// caller's eIDAS serial_number claim (the key that matches a caller to a signer slot).
// Requests without scopes are rejected 401 — mirroring the production contract.
func TestAuthMiddleware() azugo.RequestHandlerFunc {
	return func(next azugo.RequestHandler) azugo.RequestHandler {
		return func(ctx *azugo.Context) {
			scopes := ctx.Header.Get("X-Test-Scopes")
			if scopes == "" {
				ctx.StatusCode(401)
				ctx.Text("unauthorized")

				return
			}

			sub := ctx.Header.Get("X-Test-Sub")
			if sub == "" {
				sub = "svc:test-client"
			}

			claims := map[string]token.ClaimStrings{
				"sub":   {sub},
				"scope": {scopes},
			}
			if serial := ctx.Header.Get("X-Test-Serial"); serial != "" {
				claims["serial_number"] = token.ClaimStrings{serial}
			}

			ctx.SetUser(user.New(claims))
			next(ctx)
		}
	}
}
