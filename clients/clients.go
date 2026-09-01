// Package clients holds the outbound HTTP clients the envelope service uses. The
// only collaborator is the document service: at attach time the envelope validates
// a document reference (it exists and is owned by the signing user) and reads its
// content hash to pin. The document service owner-filters on the user subject, so
// the call goes out ON BEHALF OF the user via token exchange — never with the
// envelope service's own identity, which would let it see documents the user does
// not own. A call without a subject token fails closed.
package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gmb-lib/go-authbyte/authclient"
)

// Doer issues background DPoP requests to a collaborator. Reads go ON BEHALF OF
// the end user (token exchange) so the callee owner-filters on them; the
// document-chain ACL grant goes with the envelope's OWN identity (it is the
// sharing authority, not acting for a user). *authclient.Client satisfies both;
// tests inject a stub.
type Doer interface {
	DoService(ctx context.Context, audience, scope, method, fullURL string, reqHeader http.Header, body []byte) (*authclient.BackgroundResponse, error)
	DoServiceOnBehalf(ctx context.Context, audience, scope, subjectSub, subjectToken, method, fullURL string, reqHeader http.Header, body []byte) (*authclient.BackgroundResponse, error)
}

// OnBehalf carries the end-user identity a document call acts for: the user's
// subject (the delegated-token cache key) and the raw inbound token to exchange.
// A call without a subject token cannot reach a user-owned document — the client
// fails closed rather than falling back to the service's own identity.
type OnBehalf struct {
	Sub   string
	Token string
}

// HTTPError is returned when a collaborator responds with a non-2xx status; it
// carries the status so callers can map it onto their own response.
type HTTPError struct {
	Service    string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: upstream status %d: %s", e.Service, e.StatusCode, e.Body)
}

// doJSONOnBehalf issues a request acting on behalf of the end user (token
// exchange) so the callee owner-filters on the user, and decodes the JSON response
// into out when non-nil. It fails closed when no subject token is present.
func doJSONOnBehalf(ctx context.Context, d Doer, service, audience, scope, method, url string, obo OnBehalf, out any) error {
	if obo.Token == "" {
		return fmt.Errorf("%s: missing on-behalf-of subject token", service)
	}

	resp, err := d.DoServiceOnBehalf(ctx, audience, scope, obo.Sub, obo.Token, method, url, http.Header{}, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Service: service, StatusCode: resp.StatusCode, Body: string(resp.Body)}
	}
	if out != nil && len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return fmt.Errorf("%s: decode response: %w", service, err)
		}
	}

	return nil
}
