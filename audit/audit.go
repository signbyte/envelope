// Package audit records the envelope/workflow service's audit and notification
// signals for the two regimes the service participates in:
//
//   - GDPR personal-data-access audit, via go-gdpr-audit to the access-audit
//     service. Recipient and signer identities on an envelope are personal data,
//     so the transitions that process them — create, send, decline — write an
//     access record naming the people whose data was touched, the lawful basis,
//     and the purpose. The data subjects are pseudonymous internal identity
//     references (the owner subject and any slot identity references), never
//     national identifiers or contact details. Optional: wired only when the
//     access-audit service is configured.
//   - NIS2 security telemetry, to the SIEM. One event: the retention sweep, which
//     is the only thing this service does that ERASES personal data. An erasure
//     that leaves no trace of having happened cannot be shown to have happened, so
//     the sweep records what it removed and under which windows — and deliberately
//     records no identities, because naming the people whose data was erased would
//     re-create in the security stream exactly what the erasure removed.
//   - Workflow lifecycle events, to the broker, for the notification consumer.
//     Each state change (created, sent, slot-signed, completed, declined,
//     cancelled, expired, reopened) publishes a lifecycle event carrying the envelope
//     reference and the resulting status — enough for a downstream consumer to
//     notify the parties; the recipient fan-out itself is the consumer's job, so
//     the events stay free of contact data. Published with a broker connection
//     when one is configured, otherwise written to the development log transport.
//
// The service deliberately does NOT write the signing-evidence audit chain: a
// slot's cryptographic signature evidence is the signing service's record, and
// this service only references it. So the slot-signed transition emits a lifecycle
// event for notification, not a signing-evidence record.
//
// Every method is safe to call on a nil Recorder and on a Recorder whose channels
// are unconfigured (each channel then no-ops), so the request path never branches
// on whether auditing is wired. Recording is non-fatal: a failure to persist an
// access record or publish an event is logged and the transition still succeeds —
// failing a user's workflow action on audit back-pressure would be the wrong trade.
package audit

import (
	"context"
	"time"

	"azugo.io/azugo"
	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"github.com/gmb-lib/go-gdpr-audit/gdpr"
	"github.com/gmb-lib/go-platform-kit/broker"
	"github.com/gmb-lib/go-sec-events/secevents"
)

// Workflow lifecycle event types published for the notification consumer.
const (
	EventCreated    = "envelope.created"
	EventSent       = "envelope.sent"
	EventSlotSigned = "envelope.slot_signed"
	EventCompleted  = "envelope.completed"
	EventDeclined   = "envelope.declined"
	EventCancelled  = "envelope.cancelled"
	EventExpired    = "envelope.expired"
	// EventReopened: a completed envelope taken back to draft so a further signature
	// joins the workflow that already covers the container. A lifecycle event like the
	// rest, and one worth its own type: afterwards the envelope looks like any other
	// draft, so without this the record would show a completed envelope becoming
	// sendable again with nothing saying who did that or when.
	EventReopened = "envelope.reopened"
)

// NIS2 security event types.
const (
	// EventRetentionSwept records one completed retention sweep — the accountability
	// record for the erasure it performed.
	EventRetentionSwept = "envelope.retention_swept"
)

// resourceEnvelope is the resource type the events and access records concern.
const resourceEnvelope = "envelope"

// Recorder emits security telemetry, GDPR access records and workflow lifecycle
// events. sec (NIS2), gdpr (access audit) and pub (lifecycle events) are each
// optional: a nil channel no-ops.
type Recorder struct {
	sec   *secevents.Emitter
	gdpr  *gdpr.Client
	pub   *broker.Publisher
	topic string
	log   *zap.Logger
}

// New builds a Recorder. sec, gdprClient and pub may be nil (that channel no-ops);
// log may be nil.
func New(sec *secevents.Emitter, gdprClient *gdpr.Client, pub *broker.Publisher, topic string, log *zap.Logger) *Recorder {
	if log == nil {
		log = zap.NewNop()
	}

	return &Recorder{sec: sec, gdpr: gdprClient, pub: pub, topic: topic, log: log}
}

// logger returns the request-correlated logger when a request context is present —
// so a fallback/diagnostic line is joinable to its request by correlation id +
// trace id — else the component logger for a context-free background path.
func (r *Recorder) logger(ctx *azugo.Context) *zap.Logger {
	if ctx != nil {
		return ctx.Log()
	}

	return r.log
}

// Created records envelope creation: a personal-data-access record for the owner
// (and any identities named on the initial slots) and a created lifecycle event.
func (r *Recorder) Created(ctx *azugo.Context, owner, envelopeID string, slotIdentities []string) {
	r.access(ctx, owner, envelopeID, broker.OpCreate, slotIdentities)
	r.domain(ctx, EventCreated, ownerActor(owner), envelopeID, broker.OpCreate, "draft", nil)
}

// Sent records send: the recipients' personal data is processed as the envelope
// goes out, so an access record names them, and a sent lifecycle event follows.
func (r *Recorder) Sent(ctx *azugo.Context, owner, envelopeID string, slotIdentities []string) {
	r.access(ctx, owner, envelopeID, broker.OpUpdate, slotIdentities)
	r.domain(ctx, EventSent, ownerActor(owner), envelopeID, broker.OpUpdate, "sent", nil)
}

// SlotSigned records that the signing service finalized a slot: a slot-signed
// lifecycle event, and — when the envelope rolled up to completed — a completed
// event. The signing evidence itself is the signing service's audit record, not
// this service's, so no access or signing-evidence record is written here. actorID
// is the calling signing service.
func (r *Recorder) SlotSigned(ctx *azugo.Context, actorID, envelopeID, slotID, signatureID, envelopeStatus string) {
	attrs := map[string]any{"slot_id": slotID}
	if signatureID != "" {
		attrs["signature_id"] = signatureID
	}
	r.domain(ctx, EventSlotSigned, serviceActor(actorID), envelopeID, broker.OpSign, "signed", attrs)
	if envelopeStatus == "completed" {
		r.domain(ctx, EventCompleted, serviceActor(actorID), envelopeID, broker.OpUpdate, "completed", nil)
	}
}

// Declined records a signer declining a slot: an access record (the decline is an
// action on the signer's personal data) and a declined lifecycle event.
func (r *Recorder) Declined(ctx *azugo.Context, owner, envelopeID, slotID string) {
	r.access(ctx, owner, envelopeID, broker.OpUpdate, nil)
	r.domain(ctx, EventDeclined, ownerActor(owner), envelopeID, broker.OpUpdate, "declined", map[string]any{"slot_id": slotID})
}

// Cancelled records the owner cancelling an envelope: a lifecycle event only — a
// cancel processes no new personal data.
func (r *Recorder) Cancelled(ctx *azugo.Context, owner, envelopeID string) {
	r.domain(ctx, EventCancelled, ownerActor(owner), envelopeID, broker.OpUpdate, "cancelled", nil)
}

// Reopened records that the owner took a completed envelope back to draft for a
// further signature. The interesting fact for anyone reading the trail afterwards is
// that the envelope's completion was not the end of it.
func (r *Recorder) Reopened(ctx *azugo.Context, owner, envelopeID string) {
	r.domain(ctx, EventReopened, ownerActor(owner), envelopeID, broker.OpUpdate, "reopened", nil)
}

// Expired records that the retention sweep took an envelope past its own deadline:
// an expired lifecycle event, so whoever notifies the parties learns the envelope is
// closed. Background work, so there is no request to correlate it to and no new
// personal data is processed — no access record belongs here.
func (r *Recorder) Expired(envelopeID string) {
	r.background(EventExpired, envelopeID, broker.OpUpdate, "expired")
}

// RetentionSwept records one completed retention sweep. This is the service's only
// erasure, so it is the one act that must leave a trace of itself: how many
// envelopes closed on their own deadline, how many were deleted, and the windows
// that decided it — enough to show later that the policy ran and what it did.
//
// It names no envelope and no person on purpose. The rows this sweep removed held a
// signer's identity code and name; writing those into the security stream would put
// back, in another place, exactly what the erasure took out. The signing evidence
// chain remains the record of which signings happened.
//
// Nothing is emitted for a sweep that removed nothing: an hourly "erased 0" is noise
// in a SIEM, and the sweep's own completion is already in the service log.
func (r *Recorder) RetentionSwept(expired, terminalDeleted, draftsDeleted, awaitingHorizon int, grace, draftTTL time.Duration) {
	erased := terminalDeleted + draftsDeleted
	if expired+erased == 0 {
		return
	}

	r.security(EventRetentionSwept, secevents.SeverityInfo, broker.OpDelete, map[string]any{
		"expired":          expired,
		"terminal_deleted": terminalDeleted,
		"drafts_deleted":   draftsDeleted,
		// The count a rule watching for personal-data erasure reads, so it does not
		// have to know which of the stages above erase and which only transition.
		"erased": erased,
		// How many finished envelopes retention currently cannot judge, because the
		// horizon their keep window is measured from was never recorded. Zero is the
		// healthy answer; anything else is data the policy is not reaching.
		"awaiting_horizon": awaitingHorizon,
		"grace":            grace.String(),
		"draft_ttl":        draftTTL.String(),
	})
}

// security emits one NIS2 security event from background work — the sweep has no
// request, and the library's background path is what serves that: same tagging,
// sanitizing, stamping and rendered shape as a request-path event, minus the
// correlation ids there is no request to take.
func (r *Recorder) security(eventType string, sev secevents.Severity, op broker.Operation, attrs map[string]any) {
	if r == nil || r.sec == nil {
		return
	}

	attrs[secevents.AttrSeverity] = string(sev)
	ev := &broker.Envelope{
		EventType:  eventType,
		Categories: []broker.Category{broker.CategorySecurity},
		Operation:  op,
		Outcome:    broker.OutcomeSuccess,
		Attributes: attrs,
	}

	if err := r.sec.EmitBackground(context.Background(), ev); err != nil {
		r.log.Error("security event emission failed",
			zap.String("event_type", eventType), zap.Error(err))
	}
}

// background publishes one lifecycle event from work that has no request behind it.
// It stamps the envelope itself and publishes from a plain context: the request-path
// publish reads correlation ids off the request, which a background caller does not
// have. Best-effort, like every other event this recorder emits.
func (r *Recorder) background(eventType, envelopeID string, op broker.Operation, status string) {
	if r == nil || r.pub == nil {
		return
	}

	ev := &broker.Envelope{
		EventID:    ulid.Make().String(),
		OccurredAt: time.Now().UTC(),
		EventType:  eventType,
		Categories: []broker.Category{broker.CategorySigning},
		Resource:   &broker.Resource{Type: resourceEnvelope, ID: envelopeID},
		Operation:  op,
		Outcome:    broker.OutcomeSuccess,
		Attributes: map[string]any{"status": status},
	}
	if err := r.pub.PublishStamped(context.Background(), r.topic, ev); err != nil {
		r.log.Warn("envelope lifecycle event not published (non-fatal)",
			zap.String("event_type", eventType), zap.Error(err))
	}
}

// access writes one GDPR personal-data-access record naming the data subjects
// (owner plus any non-empty slot identity references). Routine and fail-open: a
// delivery failure is logged, never propagated.
func (r *Recorder) access(ctx *azugo.Context, owner, envelopeID string, op broker.Operation, slotIdentities []string) {
	if r == nil || r.gdpr == nil {
		return
	}

	subjects := dataSubjects(owner, slotIdentities)
	if len(subjects) == 0 {
		return
	}

	err := r.gdpr.EnvelopeAccessed(ctx, gdpr.Access{
		Actor:        broker.Actor{ID: owner, Type: "user"},
		DataSubjects: subjects,
		Resource:     broker.Resource{Type: gdpr.ResourceEnvelope, ID: envelopeID},
		Operation:    op,
		LawfulBasis:  gdpr.BasisContract,
		Purpose:      gdpr.PurposeSigning,
		Channel:      gdpr.ChannelInteractive,
	})
	if err != nil {
		r.logger(ctx).Warn("envelope access record not persisted (non-fatal)", zap.Error(err))
	}
}

// domain publishes one workflow lifecycle event. Best-effort: a publish failure is
// logged, never propagated.
func (r *Recorder) domain(ctx *azugo.Context, eventType string, actor *broker.Actor, envelopeID string, op broker.Operation, status string, attrs map[string]any) {
	if r == nil || r.pub == nil {
		return
	}

	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs["status"] = status

	ev := &broker.Envelope{
		EventType:  eventType,
		Categories: []broker.Category{broker.CategorySigning},
		Actor:      actor,
		Resource:   &broker.Resource{Type: resourceEnvelope, ID: envelopeID},
		Operation:  op,
		Outcome:    broker.OutcomeSuccess,
		Attributes: attrs,
	}
	if err := r.pub.Publish(ctx, r.topic, ev); err != nil {
		r.logger(ctx).Warn("envelope lifecycle event not published (non-fatal)",
			zap.String("event_type", eventType), zap.Error(err))
	}
}

// ownerActor is the owner's delegated action (a user actor), or nil when unknown.
func ownerActor(owner string) *broker.Actor {
	if owner == "" {
		return nil
	}

	return &broker.Actor{ID: owner, Type: "user"}
}

// serviceActor is a calling service (the signing service on the slot-signed
// callback), or nil when unknown.
func serviceActor(id string) *broker.Actor {
	if id == "" {
		return nil
	}

	return &broker.Actor{ID: id, Type: "service"}
}

// dataSubjects returns the deduplicated, non-empty set of pseudonymous identity
// references an access touched: the owner first, then any slot identity references.
func dataSubjects(owner string, identities []string) []string {
	seen := make(map[string]bool, len(identities)+1)
	out := make([]string, 0, len(identities)+1)
	for _, s := range append([]string{owner}, identities...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}

	return out
}
