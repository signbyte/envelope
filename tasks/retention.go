// Package tasks holds the envelope service's background work. The retention task is
// the envelope's own exit: on a schedule it takes envelopes past their deadline to
// expired, deletes finished envelopes once their keep window has run out, and
// deletes drafts nobody ever sent.
//
// It exists because a finished envelope has no purpose left to serve and is not
// harmless to keep. Its documents' bytes are gone on their own clock, the signed
// container belongs to the signer, and the signing acts live in the evidence chain,
// which references the envelope by id and does not need the row. What is left is
// personal data — a signer's identity code, a signer's name, the owner's subject —
// held past the purpose it was collected for, in what is the largest schema in the
// database. So the row goes, on a clock the deployment sets.
package tasks

import (
	"context"
	"time"

	"azugo.io/core"
	"go.uber.org/zap"

	"github.com/signbyte/envelope/audit"
	"github.com/signbyte/envelope/settle"
	"github.com/signbyte/envelope/store"
)

// RetentionConfig wires the retention task. Grace is how long a finished envelope
// stays readable AFTER its documents stop being downloadable — the horizon itself is
// not configured, it is recorded per envelope at the terminal transition from the
// service that owns the documents, so the two clocks cannot drift apart. DraftTTL is
// how long an unsent draft survives. Either at zero disables that stage; with both at
// zero and no expiring to do the task does nothing, which is a deployment's choice.
type RetentionConfig struct {
	Store store.Store
	// Documents is what the repair stage settles against. Nil disables that stage:
	// the sweep still runs, but envelopes with no recorded horizon stay unjudgeable
	// and the standing count only grows, so a deployment is told about it once.
	Documents settle.Documents
	Audit     *audit.Recorder
	Interval  time.Duration
	Batch     int
	Grace     time.Duration
	DraftTTL  time.Duration
	Logger    *zap.Logger
}

type retentionTask struct {
	cfg    RetentionConfig
	ticker *time.Ticker
	stop   chan bool
}

// NewRetentionTask returns the retention sweep task.
func NewRetentionTask(cfg RetentionConfig) core.Tasker {
	return &retentionTask{cfg: cfg}
}

func (t *retentionTask) Name() string { return "envelope-retention" }

func (t *retentionTask) Start(ctx context.Context) error {
	t.stop = make(chan bool)
	t.ticker = time.NewTicker(t.cfg.Interval)

	go func() {
		t.runOnce(ctx) // initial sweep on start
		for {
			select {
			case <-t.stop:
				return
			case <-t.ticker.C:
				t.runOnce(ctx)
			}
		}
	}()

	return nil
}

func (t *retentionTask) Stop() {
	if t.ticker != nil {
		t.ticker.Stop()
		t.stop <- true
		t.ticker = nil
	}
}

// runOnce performs one sweep, repeating while a stage keeps filling its batch so a
// backlog drains without one long pass holding the table. Each repeat is guaranteed
// to make progress: every row a pass touches stops qualifying for the stage that
// touched it.
func (t *retentionTask) runOnce(ctx context.Context) {
	batch := t.cfg.Batch
	if batch <= 0 {
		batch = defaultBatch
	}

	var expired, terminal, drafts, awaiting, settled int
	for {
		// Repair before sweeping, not after: an envelope the last pass expired has
		// no horizon yet, and until it has one the delete stage cannot see it. Doing
		// it first also makes the count the sweep reports the residue AFTER the
		// repair — which is the number worth alarming on, rather than one that
		// includes everything about to be fixed.
		repaired := t.settleUnjudged(ctx, batch)
		settled += repaired

		now := time.Now().UTC()
		window := store.RetentionWindow{
			// Expiring is judged at the clock, not at a window: an envelope carries
			// its own deadline, and the sweep is only what notices it has passed.
			ExpireBefore: now,
			Batch:        batch,
		}
		if t.cfg.Grace > 0 {
			window.TerminalBefore = now.Add(-t.cfg.Grace)
		}
		if t.cfg.DraftTTL > 0 {
			window.DraftBefore = now.Add(-t.cfg.DraftTTL)
		}

		res, err := t.cfg.Store.SweepRetention(ctx, window)
		if err != nil {
			t.log().Error("envelope retention sweep failed", zap.Error(err))

			return
		}

		// Each expiry is announced so whoever notifies the parties learns the
		// envelope closed on its own deadline rather than by anyone's action.
		for _, id := range res.Expired {
			t.cfg.Audit.Expired(id)
		}

		expired += res.ExpiredCount
		terminal += res.TerminalDeleted
		drafts += res.DraftsDeleted
		// Not summed: it is a standing count of the rows retention cannot currently
		// judge, so the last pass's view is the current one.
		awaiting = res.AwaitingHorizon

		if repaired < batch && res.ExpiredCount < batch &&
			res.TerminalDeleted < batch && res.DraftsDeleted < batch {
			break
		}
	}

	if expired+terminal+drafts+settled > 0 {
		t.log().Info("envelope retention sweep complete",
			zap.Int("expired", expired),
			zap.Int("terminal_deleted", terminal),
			zap.Int("drafts_deleted", drafts),
			// Bookkeeping, not erasure — so it stays on the service log and off the
			// security record, which answers what was destroyed.
			zap.Int("settled", settled))
	}

	// A finished envelope whose documents' horizon was never recorded cannot be
	// judged, so retention has quietly stopped applying to it. Said out loud on
	// every sweep that sees one, because the alternative is an absence nobody
	// notices until the rows have piled up.
	if awaiting > 0 {
		t.log().Warn("terminal envelopes awaiting a retention horizon — they will not be retired until one is recorded",
			zap.Int("awaiting_horizon", awaiting))
	}

	// The accountability record for what this sweep erased. Emitted once per sweep
	// rather than once per pass, so a backlog draining over several passes reads as
	// the single act of policy it is. It carries counts and windows, never an
	// identity — see the recorder for why.
	t.cfg.Audit.RetentionSwept(expired, terminal, drafts, awaiting, t.cfg.Grace, t.cfg.DraftTTL)
}

// settleUnjudged repairs terminal envelopes whose retention horizon was never
// recorded, and returns how many it settled.
//
// Without this stage the sweep has an exit it can open but not close. An envelope the
// sweep itself expires reaches a terminal state with no request behind it, so nothing
// read its documents; the delete stage requires a horizon; and the row would then
// stay forever — the stalled envelope being exactly the case this whole task exists
// for. The same repair covers the two other ways a row lands there: a read that
// failed at the transition, and a row older than the horizon being recorded at all.
//
// One envelope failing does not stop the others: each is independent, and a failure
// leaves the row where it was — on the list, for the next pass.
func (t *retentionTask) settleUnjudged(ctx context.Context, batch int) int {
	if t.cfg.Documents == nil {
		return 0
	}

	rows, err := t.cfg.Store.ListUnsettledTerminal(ctx, batch)
	if err != nil {
		t.log().Error("could not read which terminal envelopes are awaiting a retention horizon", zap.Error(err))

		return 0
	}

	settled := 0
	for _, e := range rows {
		if err := settle.Terminal(ctx, t.cfg.Documents, t.cfg.Store, t.log(), e.ID, e.Documents); err != nil {
			t.log().Error("could not settle a terminal envelope against its documents — it stays unretired until a later pass",
				zap.String("envelope", e.ID), zap.Error(err))

			continue
		}
		settled++
	}

	return settled
}

// defaultBatch is the fallback for a task wired without one, so a misconfiguration
// sweeps in sane steps rather than in one unbounded pass.
const defaultBatch = 500

func (t *retentionTask) log() *zap.Logger {
	if t.cfg.Logger != nil {
		return t.cfg.Logger
	}

	return zap.NewNop()
}
