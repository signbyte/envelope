package routes

import (
	"errors"
	"strings"
	"time"

	"azugo.io/azugo"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"

	pkerrors "github.com/gmb-lib/go-platform-kit/errors"

	"github.com/signbyte/envelope/clients"
	"github.com/signbyte/envelope/settle"
	"github.com/signbyte/envelope/store"
)

// createEnvelope — POST /api/v1/envelopes: create a draft envelope for the caller.
// Optional initial document references are validated on behalf of the user (and
// their content hash pinned) and optional slots are defined, exactly as the
// dedicated endpoints do.
func (r *router) createEnvelope(ctx *azugo.Context) {
	if !r.requireScope(ctx, "write") {
		return
	}

	var req createEnvelopeRequest
	if err := ctx.Body.JSON(&req); err != nil { // auto-validates
		ctx.Error(err)

		return
	}

	owner := ctx.User().ID()

	created, err := r.Store().CreateEnvelope(ctx, store.CreateEnvelopeInput{
		Owner:       owner,
		Title:       req.Title,
		OrderPolicy: req.OrderPolicy,
		Profile:     req.Profile,
		Expiry:      r.expiry(req.ExpiresAt),
	})
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	// Attach any initial document references (validated on behalf of the user).
	for _, docID := range req.Documents {
		if err := r.attachOnBehalf(ctx, created.ID, owner, docID); err != nil {
			r.mapErr(ctx, err)

			return
		}
	}

	// Define any initial slots.
	slotIDs := make([]string, 0, len(req.Slots))
	for _, s := range req.Slots {
		id, err := r.Store().AddSlot(ctx, slotInputToStore(created.ID, owner, s))
		if err != nil {
			r.mapErr(ctx, err)

			return
		}
		slotIDs = append(slotIDs, id)
	}

	r.Audit().Created(ctx, owner, created.ID, slotIdentities(req.Slots))

	ctx.StatusCode(fasthttp.StatusCreated)
	ctx.JSON(&createEnvelopeResponse{ID: created.ID, Status: created.Status, Version: created.Version, SlotIDs: slotIDs})
}

// listEnvelopes — GET /api/v1/envelopes?limit=&cursor=: the caller's envelopes,
// keyset-paged on the id. With ?documentId= it becomes a targeted lookup instead:
// the envelopes covering that one document which the caller may see — the owner
// always, a serial-matched participant on a non-draft envelope — newest first,
// unpaged (the set is naturally small). Lets a document-centric view resolve
// "which envelope carries this document?" for owners and participants alike.
func (r *router) listEnvelopes(ctx *azugo.Context) {
	if !r.requireScope(ctx, "read") {
		return
	}

	limit := 0
	if l, err := ctx.Query.IntOptional("limit"); err == nil && l != nil {
		limit = *l
	}

	if d := ctx.Query.StringOptional("documentId"); d != nil && *d != "" {
		envs, err := r.Store().FindEnvelopesForDocument(ctx, ctx.User().ID(), callerSerial(ctx), *d, limit)
		if err != nil {
			r.mapErr(ctx, err)

			return
		}
		ctx.JSON(&listResponse{Envelopes: toSummaries(envs)})

		return
	}

	cursor := ""
	if c := ctx.Query.StringOptional("cursor"); c != nil {
		cursor = *c
	}

	envs, err := r.Store().ListEnvelopes(ctx, ctx.User().ID(), cursor, limit)
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	resp := &listResponse{Envelopes: toSummaries(envs)}
	if len(envs) > 0 {
		resp.NextCursor = envs[len(envs)-1].ID
	}
	ctx.JSON(resp)
}

// listSigningTasks — GET /api/v1/signing-tasks?limit=&cursor=: the caller's signer
// inbox — non-draft envelopes where the caller is an invited signer (a slot's
// identity_ref matches the caller's authenticated eIDAS serial) whose signature is still
// outstanding. Keyed on the caller's identity, not ownership, so a co-signer (a different
// person from the owner) discovers the envelopes invited to them; envelopes the caller
// owns are excluded (they list under GET /envelopes). A caller without an authenticated
// serial simply has an empty inbox.
func (r *router) listSigningTasks(ctx *azugo.Context) {
	if !r.requireScope(ctx, "read") {
		return
	}

	serial := callerSerial(ctx)
	if serial == "" {
		ctx.JSON(&signingTasksResponse{Tasks: []signingTaskView{}})

		return
	}

	limit := 0
	if l, err := ctx.Query.IntOptional("limit"); err == nil && l != nil {
		limit = *l
	}
	cursor := ""
	if c := ctx.Query.StringOptional("cursor"); c != nil {
		cursor = *c
	}

	tasks, err := r.Store().ListSigningTasks(ctx, serial, ctx.User().ID(), cursor, limit)
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	resp := &signingTasksResponse{Tasks: toSigningTasks(tasks)}
	if len(tasks) > 0 {
		resp.NextCursor = tasks[len(tasks)-1].Envelope.ID
	}
	ctx.JSON(resp)
}

// getEnvelope — GET /api/v1/envelopes/{id}: the envelope plus its slots and
// document references, owner-filtered (404 on a miss — no resource enumeration).
func (r *router) getEnvelope(ctx *azugo.Context) {
	if !r.requireScope(ctx, "read") {
		return
	}

	view, err := r.Store().GetEnvelope(ctx, ctx.Params.String("id"), ctx.User().ID(), callerSerial(ctx))
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	ctx.JSON(toEnvelopeView(view))
}

// attachDocument — POST /api/v1/envelopes/{id}/documents: attach a document
// reference to a draft envelope. The reference is validated on behalf of the user
// (it exists and is owned by the user) and its content hash is pinned.
func (r *router) attachDocument(ctx *azugo.Context) {
	if !r.requireScope(ctx, "write") {
		return
	}

	var req attachDocumentRequest
	if err := ctx.Body.JSON(&req); err != nil { // auto-validates
		ctx.Error(err)

		return
	}

	if err := r.attachOnBehalf(ctx, ctx.Params.String("id"), ctx.User().ID(), req.DocumentID); err != nil {
		r.mapErr(ctx, err)

		return
	}

	ctx.StatusCode(fasthttp.StatusCreated)
	ctx.JSON(map[string]string{"envelopeId": ctx.Params.String("id"), "documentId": req.DocumentID})
}

// addSlot — POST /api/v1/envelopes/{id}/slots: define a signer slot on a draft
// envelope.
func (r *router) addSlot(ctx *azugo.Context) {
	if !r.requireScope(ctx, "write") {
		return
	}

	var req addSlotRequest
	if err := ctx.Body.JSON(&req); err != nil { // auto-validates
		ctx.Error(err)

		return
	}

	id, err := r.Store().AddSlot(ctx, slotInputToStore(ctx.Params.String("id"), ctx.User().ID(), req.slotInput))
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	ctx.StatusCode(fasthttp.StatusCreated)
	ctx.JSON(&slotIDResponse{ID: id})
}

// sendEnvelope — POST /api/v1/envelopes/{id}/send: move a draft envelope to sent.
// The preconditions (at least one document and at least one slot) are decided here
// in the service; the data layer applies the transition atomically with an
// optimistic-concurrency compare-and-set on the version.
func (r *router) sendEnvelope(ctx *azugo.Context) {
	if !r.requireScope(ctx, "transition") {
		return
	}

	id := ctx.Params.String("id")
	owner := ctx.User().ID()

	// Send is an owner-only transition; its read stays owner-scoped (no caller serial).
	view, err := r.Store().GetEnvelope(ctx, id, owner, "")
	if err != nil {
		r.mapErr(ctx, err)

		return
	}
	if view.Envelope.Status != "draft" {
		ctx.Error(pkerrors.NewProblem("err:envelope:invalidState",
			pkerrors.WithStatus(fasthttp.StatusConflict),
			pkerrors.WithDetail("only a draft envelope can be sent")))

		return
	}
	if len(view.Documents) == 0 || len(view.Slots) == 0 {
		ctx.Error(pkerrors.NewProblem("err:envelope:incomplete",
			pkerrors.WithStatus(fasthttp.StatusUnprocessableEntity),
			pkerrors.WithDetail("an envelope needs at least one document and one slot before it can be sent")))

		return
	}

	// Grant each invited participant standing access (read + co-sign) to each
	// attached document's chain BEFORE the transition, so a sent envelope always
	// has its ACL populated and a co-signer can read + sign the shared document.
	// The envelope is the sharing authority — a documents:grant service call.
	if err := r.grantParticipantAccess(ctx, view); err != nil {
		r.mapErr(ctx, err)

		return
	}

	// Freeze the chains' signed result for the signing window (it opens at the
	// workflow's terminal transition). BEFORE the transition and fail-closed: a
	// send whose freeze cannot be recorded does not go out (the grants above are
	// idempotent, so a retry is safe).
	//
	// EXCEPT on a further-signature round. An envelope that already carries a signed
	// slot has been through a round: its container holds complete signatures and is
	// already downloadable, so freezing it again would take a released result back
	// from the people who had it — punishing them for someone else adding a
	// signature. The predicate is exact rather than convenient: the freeze exists to
	// stop a HALF-signed result being downloaded, it is applied once per send rather
	// than re-evaluated per signature, and a first round has no signed slot, so
	// nothing about the half-signed case is weakened. What an N+1th signature cannot
	// do is invalidate the N that are already there.
	if !hasSignedSlot(view) {
		for _, d := range view.Documents {
			if err := r.Documents().SetResultFreeze(ctx, d.DocumentID, true); err != nil {
				r.mapErr(ctx, err)

				return
			}
		}
	}

	res, err := r.Store().ApplyTransition(ctx, id, owner, "sent", view.Envelope.Version)
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	r.Audit().Sent(ctx, owner, id, viewSlotIdentities(view))

	ctx.JSON(&transitionResponse{ID: res.ID, Status: res.Status, Version: res.Version})
}

// cancelEnvelope — POST /api/v1/envelopes/{id}/cancel: move an envelope to
// cancelled (compare-and-set on the current version).
func (r *router) cancelEnvelope(ctx *azugo.Context) {
	if !r.requireScope(ctx, "transition") {
		return
	}

	id := ctx.Params.String("id")
	owner := ctx.User().ID()

	// Cancel is an owner-only transition; its read stays owner-scoped (no caller serial).
	view, err := r.Store().GetEnvelope(ctx, id, owner, "")
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	res, err := r.Store().ApplyTransition(ctx, id, owner, "cancelled", view.Envelope.Version)
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	// Terminal — lift the download freeze on the envelope's chains.
	docIDs := make([]string, 0, len(view.Documents))
	for _, d := range view.Documents {
		docIDs = append(docIDs, d.DocumentID)
	}
	r.settleTerminal(ctx, id, docIDs)

	r.Audit().Cancelled(ctx, owner, id)

	ctx.JSON(&transitionResponse{ID: res.ID, Status: res.Status, Version: res.Version})
}

// reopenEnvelope — POST /api/v1/envelopes/{id}/reopen: take a COMPLETED envelope back
// to draft so a further signature is added to the workflow that already covers the
// container, rather than a second envelope being minted over the same chain (the
// envelope is the chain's workflow home once it exists).
//
// Completed only. Declined, cancelled and expired are closed for a reason that is not
// "another signature is coming", and reopening one would rewrite that record.
//
// The slots stay as they are: the signed ones ARE the record of the previous round,
// and the new slot is added beside them. Owner-only, like every other transition, and
// version-CAS so a concurrent act cannot be lost.
//
// It deliberately does NOT open signing. Chain access is granted at SEND, over the
// whole (documents x slots) set — add_slot grants nothing — so a round that reached
// signing without passing through send again would leave a slot whose signer has no
// access to the document. Reopen hands back a draft; the caller adds its slot and
// sends, and that send is what grants.
func (r *router) reopenEnvelope(ctx *azugo.Context) {
	if !r.requireScope(ctx, "transition") {
		return
	}

	id := ctx.Params.String("id")
	owner := ctx.User().ID()

	// Owner-scoped read (no caller serial): reopening is the owner's act.
	view, err := r.Store().GetEnvelope(ctx, id, owner, "")
	if err != nil {
		r.mapErr(ctx, err)

		return
	}
	if view.Envelope.Status != "completed" {
		ctx.Error(pkerrors.NewProblem("err:envelope:invalidState",
			pkerrors.WithStatus(fasthttp.StatusConflict),
			pkerrors.WithDetail("only a completed envelope can be reopened for a further signature")))

		return
	}

	res, err := r.Store().ApplyTransition(ctx, id, owner, "draft", view.Envelope.Version)
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	r.Audit().Reopened(ctx, owner, id)

	ctx.JSON(&transitionResponse{ID: res.ID, Status: res.Status, Version: res.Version})
}

// hasSignedSlot reports whether any slot on the envelope has already been signed —
// i.e. whether this envelope has been through a signing round. Used to tell a first
// send from a further-signature send.
func hasSignedSlot(view *store.EnvelopeView) bool {
	for _, s := range view.Slots {
		if s.Status == "signed" {
			return true
		}
	}

	return false
}

// slotEligible — GET /api/v1/envelopes/{id}/slots/{slot}/eligible: the order-policy
// precondition the backend-for-frontend checks before triggering a slot's signing.
func (r *router) slotEligible(ctx *azugo.Context) {
	if !r.requireScope(ctx, "read") {
		return
	}

	eligible, err := r.Store().SlotEligible(ctx, ctx.Params.String("id"), ctx.Params.String("slot"), ctx.User().ID(), callerSerial(ctx))
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	ctx.JSON(&eligibleResponse{Eligible: eligible})
}

// setSlotJob — POST /api/v1/envelopes/{id}/slots/{slot}/job: record the signing
// job id on a slot when signing starts, advancing the envelope on first signing.
func (r *router) setSlotJob(ctx *azugo.Context) {
	if !r.requireScope(ctx, "transition") {
		return
	}

	var req setSlotJobRequest
	if err := ctx.Body.JSON(&req); err != nil { // auto-validates
		ctx.Error(err)

		return
	}

	if err := r.Store().SetSlotJob(ctx, ctx.Params.String("id"), ctx.Params.String("slot"), ctx.User().ID(), callerSerial(ctx), req.JobID); err != nil {
		r.mapErr(ctx, err)

		return
	}

	ctx.JSON(map[string]string{"id": ctx.Params.String("slot"), "jobId": req.JobID})
}

// slotSigned — POST /api/v1/envelopes/{id}/slots/{slot}/signed: the Signing
// Orchestrator reports a slot finalized. Not owner-scoped — the signing service,
// not the owner, calls it, gated by the transition scope. Idempotent: a replayed
// callback is a no-op success. Advances the envelope and rolls it up to completed
// once every slot is signed.
func (r *router) slotSigned(ctx *azugo.Context) {
	if !r.requireScope(ctx, "transition") {
		return
	}

	var req slotSignedRequest
	if err := ctx.Body.JSON(&req); err != nil { // auto-validates
		ctx.Error(err)

		return
	}

	res, err := r.Store().MarkSlotSigned(ctx, ctx.Params.String("id"), ctx.Params.String("slot"), req.SignatureID, req.SignedDocRef, req.JobID)
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	// The last signature rolls the envelope to completed — terminal, so lift
	// the download freeze: the signed result opens to the parties.
	if res.EnvelopeStatus == "completed" {
		r.settleTerminal(ctx, ctx.Params.String("id"), res.DocIDs)
	}

	r.Audit().SlotSigned(ctx, ctx.User().ID(), ctx.Params.String("id"), ctx.Params.String("slot"), req.SignatureID, res.EnvelopeStatus)

	ctx.JSON(&signedResponse{ID: res.ID, Status: res.Status, Idempotent: res.Idempotent, EnvelopeStatus: res.EnvelopeStatus})
}

// declineSlot — POST /api/v1/envelopes/{id}/slots/{slot}/decline: a signer
// declines, driving the envelope to declined.
func (r *router) declineSlot(ctx *azugo.Context) {
	if !r.requireScope(ctx, "transition") {
		return
	}

	res, err := r.Store().DeclineSlot(ctx, ctx.Params.String("id"), ctx.Params.String("slot"), ctx.User().ID(), callerSerial(ctx))
	if err != nil {
		r.mapErr(ctx, err)

		return
	}

	// A decline drives the envelope terminal — lift the download freeze.
	switch res.EnvelopeStatus {
	case "declined", "cancelled", "completed", "expired":
		r.settleTerminal(ctx, ctx.Params.String("id"), res.DocIDs)
	}

	r.Audit().Declined(ctx, ctx.User().ID(), ctx.Params.String("id"), ctx.Params.String("slot"))

	ctx.JSON(map[string]string{"id": ctx.Params.String("slot"), "status": "declined"})
}

// captureSignerName — POST /api/v1/envelopes/{id}/slots/{slot}/name: record the caller's
// display name on their own slot (write-once). The name is supplied by the portal from the
// caller's authenticated session; the store enforces that a caller may only name their own
// slot (owner, or a participant matched by serial). Idempotent — already-named or
// not-entitled is a success no-op.
func (r *router) captureSignerName(ctx *azugo.Context) {
	if !r.requireScope(ctx, "transition") {
		return
	}

	var req captureSignerNameRequest
	if err := ctx.Body.JSON(&req); err != nil { // auto-validates
		ctx.Error(err)

		return
	}

	if err := r.Store().CaptureSignerName(ctx, ctx.Params.String("id"), ctx.Params.String("slot"), ctx.User().ID(), callerSerial(ctx), req.Name); err != nil {
		r.mapErr(ctx, err)

		return
	}

	ctx.JSON(map[string]string{"id": ctx.Params.String("slot")})
}

// attachOnBehalf validates a document reference on behalf of the user (it exists
// and is owned by the user), pins its content hash, and attaches it. It fails
// closed: without the document client configured the operation reports not-ready,
// and without a subject token the on-behalf call cannot reach a user document.
func (r *router) attachOnBehalf(ctx *azugo.Context, envelopeID, owner, documentID string) error {
	docs := r.Documents()
	if docs == nil {
		return errNotReady
	}

	meta, err := docs.Validate(ctx, documentID, clients.OnBehalf{Sub: owner, Token: subjectToken(ctx)})
	if err != nil {
		return err
	}
	// Defense in depth: the document service already owner-filters, but confirm the
	// returned owner matches the caller and the hash is present before pinning.
	if meta.Owner != "" && meta.Owner != owner {
		return store.ErrNotFound
	}
	if meta.ContentHash == "" {
		return errMissingHash
	}

	return r.Store().AttachDocument(ctx, envelopeID, owner, documentID, meta.ContentHash)
}

// grantParticipantAccess grants every invited slot serial standing access (read +
// co-sign) to each attached document's chain, so co-signers can read + co-sign the
// shared document once the envelope is sent. Idempotent (a re-send re-grants); the
// owner's own slot is harmless (they already hold the creator entry). Fails closed
// when the document collaborator is unconfigured.
func (r *router) grantParticipantAccess(ctx *azugo.Context, view *store.EnvelopeView) error {
	docs := r.Documents()
	if docs == nil {
		return errNotReady
	}
	for _, d := range view.Documents {
		for _, s := range view.Slots {
			if s.IdentityRef == "" {
				continue
			}
			if err := docs.GrantChainACL(ctx, d.DocumentID, s.IdentityRef); err != nil {
				return err
			}
		}
	}

	return nil
}

// settleTerminal closes an envelope out against its documents: it clears the
// download freeze on each chain, and records how long those documents remain
// downloadable so the retention sweep knows how long this envelope must stay.
//
// Best-effort BY DESIGN: the transition is already committed. A failed settle leaves
// the chain locked-but-visible (never silently open) and the envelope unjudgeable by
// the sweep, which then waits rather than guessing — and the background repair picks
// it up on a later pass, so nothing is stranded permanently.
//
// The work itself lives in the settle package, because the envelope can also reach a
// terminal state with no request behind it at all — its own deadline passing — and
// what a workflow owes its documents at the end must not depend on which of the two
// got there first.
func (r *router) settleTerminal(ctx *azugo.Context, envelopeID string, docIDs []string) {
	docs := r.Documents()
	if docs == nil {
		return
	}

	if err := settle.Terminal(ctx, docs, r.Store(), ctx.Log(), envelopeID, docIDs); err != nil {
		ctx.Log().Error("could not record how long this envelope must be kept — it will not be retired until the repair pass reaches it",
			zap.String("envelope", envelopeID), zap.Error(err))
	}
}

// slotIdentities returns the non-empty identity references named on the given slot
// inputs — the people whose personal data the create processes (for the GDPR access
// record).
func slotIdentities(slots []slotInput) []string {
	out := make([]string, 0, len(slots))
	for _, s := range slots {
		if s.IdentityRef != "" {
			out = append(out, s.IdentityRef)
		}
	}

	return out
}

// viewSlotIdentities returns the non-empty identity references on a stored
// envelope's slots (for the GDPR access record on send).
func viewSlotIdentities(view *store.EnvelopeView) []string {
	out := make([]string, 0, len(view.Slots))
	for _, s := range view.Slots {
		if s.IdentityRef != "" {
			out = append(out, s.IdentityRef)
		}
	}

	return out
}

// slotInputToStore maps a slot DTO onto the store input.
func slotInputToStore(envelopeID, owner string, s slotInput) store.AddSlotInput {
	return store.AddSlotInput{
		EnvelopeID:  envelopeID,
		Owner:       owner,
		OrderIndex:  s.OrderIndex,
		IdentityRef: s.IdentityRef,
		Role:        s.Role,
		Flow:        s.Flow,
		RequiredLoA: s.RequiredLoA,
	}
}

var (
	// errNotReady marks the document collaborator as unconfigured (attach fails closed).
	errNotReady = errors.New("document collaborator is not configured")
	// errMissingHash marks a document reference whose content hash could not be read.
	errMissingHash = errors.New("document content hash unavailable")
)

// expiry resolves the deadline to store on a new envelope: what the caller asked
// for, or the service's configured default counted from now. A configured default of
// zero leaves the envelope open-ended, and an open-ended envelope simply never
// expires on its own.
func (r *router) expiry(requested string) string {
	if requested != "" {
		return requested
	}

	d := r.Config().DefaultExpiry
	if d <= 0 {
		return ""
	}

	return time.Now().UTC().Add(d).Format(time.RFC3339)
}

// requireScope enforces an envelopes:<level> scope. Inbound callers present
// service tokens; the matching grants are registered with those callers.
func (r *router) requireScope(ctx *azugo.Context, level string) bool {
	if ctx.User().HasScopeLevel("envelopes", level) {
		return true
	}

	ctx.Error(pkerrors.NewProblem("err:envelope:forbidden",
		pkerrors.WithDetail("missing envelopes:"+level+" scope")))

	return false
}

// mapErr maps a store/collaborator error onto the right HTTP status. A miss is 404
// (owner-filtered — absent-or-not-owned is indistinguishable, no enumeration leak);
// a stale-version transition is 409; a domain validation failure is 422.
func (r *router) mapErr(ctx *azugo.Context, err error) {
	var httpErr *clients.HTTPError
	switch {
	case errors.Is(err, store.ErrNotFound):
		ctx.Error(pkerrors.NewProblem("err:envelope:notFound",
			pkerrors.WithDetail("envelope or slot not found")))
	case errors.Is(err, store.ErrConflict):
		ctx.Error(pkerrors.NewProblem("err:envelope:conflict",
			pkerrors.WithDetail("the envelope changed concurrently; reload and retry")))
	case errors.Is(err, store.ErrDuplicate):
		ctx.Error(pkerrors.NewProblem("err:envelope:duplicate",
			pkerrors.WithDetail(err.Error())))
	case errors.Is(err, store.ErrSlotLimit):
		// The edition's signer count, not a transient condition: 422, and the detail
		// says what the product does rather than hinting at a setting to change.
		ctx.Error(pkerrors.NewProblem("err:envelope:slotLimit",
			pkerrors.WithStatus(fasthttp.StatusUnprocessableEntity),
			pkerrors.WithDetail("this envelope already has its signers; signing here is between two parties")))
	case errors.Is(err, store.ErrInvalid):
		// A domain validation failure is 422 (the "invalid" reason maps to 400 by
		// default, so pin the status).
		ctx.Error(pkerrors.NewProblem("err:envelope:invalid",
			pkerrors.WithStatus(fasthttp.StatusUnprocessableEntity),
			pkerrors.WithDetail(err.Error())))
	case errors.Is(err, errNotReady):
		ctx.Error(pkerrors.NewProblem("err:envelope:notConfigured",
			pkerrors.WithStatus(fasthttp.StatusServiceUnavailable),
			pkerrors.WithDetail("document validation is not configured")))
	case errors.Is(err, errMissingHash):
		ctx.Error(pkerrors.NewProblem("err:envelope:missingHash",
			pkerrors.WithStatus(fasthttp.StatusBadGateway),
			pkerrors.WithDetail("could not read the document content hash")))
	case errors.As(err, &httpErr):
		// A document the user does not own returns not-found at the document
		// service; surface it as not-found here so an unauthorized reference is
		// indistinguishable from an absent one — a deliberate produce, not a relay.
		if httpErr.StatusCode == fasthttp.StatusNotFound {
			ctx.Error(pkerrors.NewProblem("err:document:notFound",
				pkerrors.WithDetail("document not found")))

			return
		}
		// Any other downstream failure: relay the terminal problem (preserving its
		// code, source, and trace id) rather than collapsing it to a bare gateway error.
		outer := httpErr.StatusCode
		if outer >= fasthttp.StatusInternalServerError {
			outer = fasthttp.StatusBadGateway
		}
		down, _ := pkerrors.ParseProblem([]byte(httpErr.Body))
		ctx.Error(pkerrors.Relay(down, r.AppName, outer))
	default:
		// No downstream response — an internal failure; log the cause off the wire.
		ctx.Log().Error("envelope operation failed", zap.Error(err))
		ctx.Error(pkerrors.NewProblem("err:envelope:internal"))
	}
}

// callerSerial returns the caller's authenticated eIDAS identity code (the
// serial_number claim), or "" when the token carries none. It is the key that matches a
// caller to an invited signer slot — a trusted token claim, never a spoofable header —
// so a co-signer, not only the owner, can read the envelope and act on their slot.
func callerSerial(ctx *azugo.Context) string {
	return ctx.User().ClaimValue("serial_number")
}

// subjectToken returns the raw inbound access token (without its auth scheme) so
// it can be exchanged for a delegated token on the document calls made for the
// user. Inbound tokens are DPoP-bound, so the scheme is "DPoP"; "Bearer" is
// tolerated.
func subjectToken(ctx *azugo.Context) string {
	h := ctx.Header.Get("Authorization")
	if i := strings.IndexByte(h, ' '); i >= 0 {
		return strings.TrimSpace(h[i+1:])
	}

	return strings.TrimSpace(h)
}
