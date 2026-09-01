package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Documents is the client for the document service — the platform's owner of
// document bytes and canonical hashes. The envelope service reads only metadata
// (byte-free) to validate a reference and pin its content hash; the bytes
// themselves never transit the envelope service.
type Documents struct {
	doer     Doer
	baseURL  string
	audience string
}

// NewDocuments builds a document-service client over the given outbound doer.
func NewDocuments(d Doer, baseURL, audience string) *Documents {
	return &Documents{doer: d, baseURL: strings.TrimRight(baseURL, "/"), audience: audience}
}

const (
	scopeDocRead  = "documents:read"
	scopeDocGrant = "documents:grant"
)

// grantACLRequest is the body of the document-store chain-ACL grant route.
type grantACLRequest struct {
	Serial string `json:"serial"`
}

// Meta is the document-metadata projection the envelope service needs: the owner
// (to confirm the reference belongs to the signing user) and the content hash (to
// pin at attach as the "same document" invariant).
type Meta struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	ContentHash string `json:"contentHash"`
}

// Validate fetches a document's metadata on behalf of the signing user and
// confirms the reference is usable: the document service owner-filters on the user
// subject, so a document the user does not own returns not-found there (mapped to
// an HTTP error by the caller). It returns the metadata, including the content hash
// to pin. Acts on behalf of the signing user; fails closed without a subject token.
func (c *Documents) Validate(ctx context.Context, id string, obo OnBehalf) (*Meta, error) {
	url := fmt.Sprintf("%s/api/v1/documents/%s", c.baseURL, id)

	var out Meta
	if err := doJSONOnBehalf(ctx, c.doer, "document", c.audience, scopeDocRead, http.MethodGet, url, obo, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// GrantChainACL grants an invited participant standing access (read + co-sign) to
// the chain of the given document. Unlike the on-behalf reads, this is a SERVICE
// call with the envelope's OWN identity (documents:grant) — the envelope is the
// sharing authority that populates the document ACL at send. Idempotent: a
// re-send re-grants without error.
func (c *Documents) GrantChainACL(ctx context.Context, documentID, serial string) error {
	url := fmt.Sprintf("%s/api/v1/documents/%s/acl", c.baseURL, documentID)

	body, err := json.Marshal(grantACLRequest{Serial: serial})
	if err != nil {
		return fmt.Errorf("document: marshal grant: %w", err)
	}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")

	resp, err := c.doer.DoService(ctx, c.audience, scopeDocGrant, http.MethodPost, url, hdr, body)
	if err != nil {
		return fmt.Errorf("document: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Service: "document", StatusCode: resp.StatusCode, Body: string(resp.Body)}
	}

	return nil
}

// ChainRetention reads when the document's chain stops being downloadable. The
// envelope keeps its own record only as long as that download exists, and cannot
// work the instant out for itself: retention rolls forward every time a signature
// lands, so a value read at attach is already a lower bound by the time the workflow
// ends. Read at the terminal transition — the last moment retention can move — with
// the same service identity and grant scope as the rest of the chain administration.
//
// A zero instant with no live rows means the chain holds nothing any more: there is
// no download left to outlive.
func (c *Documents) ChainRetention(ctx context.Context, documentID string) (time.Time, int, error) {
	url := fmt.Sprintf("%s/api/v1/documents/%s/retention", c.baseURL, documentID)

	resp, err := c.doer.DoService(ctx, c.audience, scopeDocGrant, http.MethodGet, url, http.Header{}, nil)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("document: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return time.Time{}, 0, &HTTPError{Service: "document", StatusCode: resp.StatusCode, Body: string(resp.Body)}
	}

	var out struct {
		RetentionUntil time.Time `json:"retentionUntil"`
		LiveRows       int       `json:"liveRows"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return time.Time{}, 0, fmt.Errorf("document: decode retention: %w", err)
	}

	return out.RetentionUntil.UTC(), out.LiveRows, nil
}

// SetResultFreeze sets/clears the download freeze on the given document's chain.
// The envelope is the workflow authority: it freezes the signed result at send
// (the result opens only at the workflow's terminal transition) and lifts the
// freeze when the envelope goes terminal. Same service identity + grant scope as
// the ACL administration. Idempotent.
func (c *Documents) SetResultFreeze(ctx context.Context, documentID string, frozen bool) error {
	url := fmt.Sprintf("%s/api/v1/documents/%s/result-freeze", c.baseURL, documentID)

	body, err := json.Marshal(map[string]bool{"frozen": frozen})
	if err != nil {
		return fmt.Errorf("document: marshal freeze: %w", err)
	}
	hdr := http.Header{}
	hdr.Set("Content-Type", "application/json")

	resp, err := c.doer.DoService(ctx, c.audience, scopeDocGrant, http.MethodPost, url, hdr, body)
	if err != nil {
		return fmt.Errorf("document: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{Service: "document", StatusCode: resp.StatusCode, Body: string(resp.Body)}
	}

	return nil
}
