// Package settle holds what a workflow owes its documents at the moment it ends,
// in one place so that every path which ends a workflow does the same thing.
//
// Two obligations fall due together at a terminal transition, and they are due
// whether a person ended the envelope or the envelope ended itself:
//
//   - the download freeze taken when the envelope was sent is lifted, so the signed
//     result becomes readable by the people entitled to it;
//   - the instant the documents stop being downloadable is read and pinned on the
//     envelope, so retention knows how long the tracking page must outlive them.
//
// It is read HERE, at the transition, because this is the last moment it can move:
// document retention rolls forward on every signing act, and no signing happens on a
// terminal envelope. A value read any earlier is a lower bound, and acting on one
// would drop the tracking page while its document was still readable.
package settle

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/signbyte/envelope/clients"
)

// Documents is the part of the document service this needs: the freeze it lifts and
// the retention it reads. Both ride the service's own identity, so neither needs a
// user — which is what lets a background task settle an envelope nobody asked about.
type Documents interface {
	SetResultFreeze(ctx context.Context, documentID string, frozen bool) error
	ChainRetention(ctx context.Context, documentID string) (time.Time, int, error)
}

// Horizons is the part of the store this writes.
type Horizons interface {
	SetRetentionUntil(ctx context.Context, id string, until time.Time) error
}

// Terminal settles one envelope against its documents.
//
// Best-effort on the freeze, fail-loud on the horizon: a failed lift leaves the chain
// locked-but-visible and is logged, while a horizon that cannot be read is returned
// as an error, because recording a wrong one is worse than recording none. An
// envelope with no horizon is simply never retired — and is listed for repair, so a
// failure here is recoverable on a later pass rather than permanent.
func Terminal(ctx context.Context, docs Documents, horizons Horizons, log *zap.Logger, envelopeID string, docIDs []string) error {
	if log == nil {
		log = zap.NewNop()
	}

	var horizon time.Time
	for _, id := range docIDs {
		if err := docs.SetResultFreeze(ctx, id, false); err != nil {
			log.Error("download-freeze lift failed — the chain stays locked until re-lifted",
				zap.String("document", id), zap.Error(err))
		}

		until, live, err := docs.ChainRetention(ctx, id)
		if err != nil {
			// A document the service no longer knows puts no floor under the
			// envelope: nothing is readable, so there is nothing for the tracking
			// page to outlive. Treated as an answer rather than a failure, or a
			// chain deleted out from under an envelope would strand it forever.
			if !gone(err) {
				return err
			}

			continue
		}
		// A chain with nothing stored has no download left to outlive either, so it
		// puts no floor under the envelope; the horizon is the latest across the rest.
		if live > 0 && until.After(horizon) {
			horizon = until
		}
	}

	// An envelope carrying no live documents at all still needs a horizon, or it
	// would wait forever. Now is the honest answer: nothing is readable from this
	// moment on.
	if horizon.IsZero() {
		horizon = time.Now().UTC()
	}

	return horizons.SetRetentionUntil(ctx, envelopeID, horizon)
}

// gone reports whether the document service answered that it has no such chain.
func gone(err error) bool {
	var he *clients.HTTPError

	return errors.As(err, &he) && he.StatusCode == http.StatusNotFound
}
