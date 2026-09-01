// Package envelope is the Envelope/Workflow service: the portal's workflow brain.
// It owns the multi-document, multi-signer envelope and its signer slots, runs the
// envelope state machine with optimistic-concurrency compare-and-set, and is the
// durable record of who must sign what, in what order, and where each slot stands.
//
// It holds no document bytes (the document service owns those) and no signing
// crypto or signature evidence (the signing services own those): it references
// documents and signing jobs by plain id. A document reference is validated, at
// attach time, on behalf of the signing user so the service only ever sees the
// user's own documents, and the document's content hash is pinned then to enforce
// the "every party signs the same document" invariant.
//
// Cross-cutting concerns (logging with redaction, tracing, correlation) are
// installed once by the shared platform-kit and are never wired per-service.
package envelope

import (
	"fmt"

	"azugo.io/azugo"
	"azugo.io/azugo/server"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/gmb-lib/go-authbyte/authclient"
	"github.com/gmb-lib/go-gdpr-audit/gdpr"
	"github.com/gmb-lib/go-platform-kit/broker"
	"github.com/gmb-lib/go-platform-kit/broker/natsbroker"
	"github.com/gmb-lib/go-platform-kit/platform"
	"github.com/gmb-lib/go-sec-events/secevents"

	"github.com/signbyte/envelope/audit"
	"github.com/signbyte/envelope/clients"
	"github.com/signbyte/envelope/store"
	"github.com/signbyte/envelope/tasks"
)

// App is the envelope application container.
type App struct {
	*azugo.App

	config *Configuration

	// Durable workflow state: the envelope schema via an EXECUTE-only role (or
	// in-memory in development/test).
	store store.Store

	// Inbound service authentication (go-authbyte DPoP). Callers are the
	// backend-for-frontend (the owner's delegated actions) and the Signing
	// Orchestrator (the slot-signed callback).
	authClient *authclient.Client
	authMW     azugo.RequestHandlerFunc

	// Outbound DPoP service client — used to validate document references on behalf
	// of the user via token exchange. Nil until the document base URL is configured.
	outboundClient *authclient.Client

	// documents validates a document reference (exists + owned by the user) and
	// reads its content hash to pin at attach. Nil until the document service is
	// configured; the attach route then reports not-ready.
	documents *clients.Documents

	// gdprAudit posts GDPR personal-data-access records to the access-audit service.
	// Nil until access-audit is configured.
	gdprAudit *gdpr.Client

	// audit records GDPR access records (the PII-bearing transitions), publishes
	// workflow lifecycle events for the notification consumer, and emits the
	// security record for the retention sweep's erasure. Always present after init;
	// its channels no-op when unconfigured.
	audit *audit.Recorder
}

// New constructs the application.
func New(cmd *cobra.Command, version string) (*App, error) {
	config := NewConfiguration()

	a, err := server.New(cmd, server.Options{
		AppName:       "Envelope/Workflow service (envelope)",
		AppVer:        version,
		Configuration: config,
	})
	if err != nil {
		return nil, err
	}

	app := &App{App: a, config: config}
	if err := app.init(); err != nil {
		return nil, err
	}

	return app, nil
}

func (a *App) init() error {
	cfg := a.config

	// Platform glue FIRST: logging + redaction, tracing, correlation.
	if err := platform.Setup(a.App, platform.Options{Config: cfg.BaseConfiguration}); err != nil {
		return err
	}

	var err error

	// Durable workflow state: the envelope schema via an EXECUTE-only role, or
	// in-memory (development/test).
	if cfg.StorePostgres() {
		a.store, err = store.NewPostgres(a.BackgroundContext(), cfg.StoreDSN)
		if err != nil {
			return err
		}
	} else {
		a.Log().Warn("no store DSN configured (ENVELOPE_STORE_DSN) — using in-memory store; envelopes will NOT survive restarts (development only)")
		a.store = store.NewMemory()
	}

	// Inbound service authentication (go-authbyte DPoP): callers present
	// svc:envelope service tokens (the delegated on-behalf tokens included).

	a.authClient, err = authclient.New(cfg.Auth)
	if err != nil {
		return fmt.Errorf("envelope: auth client: %w", err)
	}
	a.authMW = a.authClient.Authenticate()

	// Outbound DPoP service client — shared by the on-behalf document reference
	// validation and the access-audit poster (each call picks its own audience +
	// scope). Built when either outbound use is configured.
	if cfg.OutboundEnabled() {
		a.outboundClient, err = authclient.New(cfg.OutboundAuthClientConfig())
		if err != nil {
			return fmt.Errorf("envelope: outbound auth client: %w", err)
		}
	}

	// Document reference validation on behalf of the user (byte-free).
	if cfg.DocumentEnabled() {
		a.documents = clients.NewDocuments(a.outboundClient, cfg.DocumentBaseURL, cfg.DocumentAudience)
	} else {
		a.Log().Warn("no document base URL set (DOCUMENT_BASE_URL) — attaching a document reference will report not-ready until it is configured")
	}

	// Audit + notification: GDPR access records to access-audit (optional) and
	// workflow lifecycle events to the broker (or the dev log transport).
	return a.initAudit()
}

// initAudit wires the GDPR personal-data-access client (when access-audit is
// configured) and the workflow lifecycle-event publisher (broker when BROKER_URL
// is set, else the development log transport), then builds the recorder.
func (a *App) initAudit() error {
	cfg := a.config

	var gc *gdpr.Client
	if cfg.AccessAuditEnabled() {
		var outbox gdpr.Outbox
		if dir := cfg.AccessAuditOutboxDir; dir != "" {
			ob, err := gdpr.NewFileOutbox(dir, gdpr.DefaultOutboxCapacity)
			if err != nil {
				return fmt.Errorf("envelope: audit outbox: %w", err)
			}
			outbox = ob
		}

		c, err := gdpr.New(
			cfg.GDPRConfig(),
			newAccessAuditPoster(a.outboundClient, cfg.AccessAuditURL, cfg.AccessAuditAudience, cfg.AccessAuditScope),
			gdpr.Options{Outbox: outbox, Logger: a.Log()},
		)
		if err != nil {
			return fmt.Errorf("envelope: gdpr-audit client: %w", err)
		}
		a.gdprAudit = c
		gc = c

		if err := a.AddTask(audit.NewDrainTask(c)); err != nil {
			return fmt.Errorf("envelope: gdpr drain task: %w", err)
		}
	} else {
		a.Log().Warn("ACCESS_AUDIT_URL not set — GDPR personal-data-access records will NOT be posted (development)")
	}

	var transport broker.Transport
	if cfg.Broker != nil && cfg.Broker.URL != "" {
		conn, err := natsbroker.Connect(natsbroker.Config{
			URL:     cfg.Broker.URL,
			TLSCert: cfg.Broker.TLSCert,
			TLSKey:  cfg.Broker.TLSKey,
			TLSCA:   cfg.Broker.TLSCA,
			Name:    cfg.ServiceName,
		})
		if err != nil {
			return fmt.Errorf("envelope: broker connect: %w", err)
		}
		transport = natsbroker.NewTransport(conn)
		a.Log().Info("envelope lifecycle events → NATS JetStream",
			zap.String("broker_url", cfg.Broker.URL), zap.String("topic", cfg.EnvelopeEventsTopic))
	} else {
		transport = newLogTransport(a.Log())
		a.Log().Warn("BROKER_URL unset — envelope lifecycle events go to the dev log transport only; set BROKER_URL to publish for the notification consumer")
	}

	// NIS2 security telemetry ships through the platform's log pipeline, like every
	// other service's — the retention sweep is the one act here that erases personal
	// data, and an erasure has to leave a record of itself. The sink carries the
	// service logger because that sweep has no request whose logger it could borrow.
	secEmitter := secevents.NewEmitter(secevents.NewLogSinkFor(a.Log()))

	a.audit = audit.New(secEmitter, gc, broker.NewPublisher(transport, cfg.ServiceName), cfg.EnvelopeEventsTopic, a.Log())

	// Retention: the envelope's own exit — expire what ran past its deadline, then
	// delete what has no purpose left to serve (a finished envelope past its keep
	// window, an abandoned draft) so signer identities and names do not outlive the
	// signing they were collected for.
	rc := tasks.RetentionConfig{
		Store:    a.store,
		Audit:    a.audit,
		Interval: cfg.RetentionSweepInterval,
		Batch:    cfg.RetentionSweepBatch,
		Grace:    cfg.RetentionGrace,
		DraftTTL: cfg.DraftTTL,
		Logger:   a.Log(),
	}
	// Assigned only when there is a client, never as a typed nil: the sweep tests
	// this field to decide whether it can settle at all, and a typed nil in an
	// interface answers "yes" and then panics on first use.
	if a.documents != nil {
		rc.Documents = a.documents
	} else {
		a.Log().Warn("no document base URL set (DOCUMENT_BASE_URL) — the retention sweep cannot settle envelopes it expires, so they will never be retired")
	}

	if err := a.AddTask(tasks.NewRetentionTask(rc)); err != nil {
		return fmt.Errorf("envelope: retention task: %w", err)
	}

	return nil
}

// Start verifies store connectivity (non-fatal) then starts the server.
func (a *App) Start() error {
	if err := a.store.Ping(a.BackgroundContext()); err != nil {
		a.Log().Warn("envelope store not reachable at start — readiness will report not-ready until it recovers", zap.Error(err))
	}

	return a.App.Start()
}

// Stop releases the store, then stops the server.
func (a *App) Stop() {
	if a.store != nil {
		a.store.Close()
	}
	a.App.Stop()
}

// Config returns the loaded configuration.
func (a *App) Config() *Configuration {
	if a.config == nil || !a.config.Ready() {
		panic("configuration is not loaded")
	}

	return a.config
}

// Store returns the durable-state store (the envelope schema).
func (a *App) Store() store.Store { return a.store }

// AuthMiddleware returns the inbound service-authentication middleware.
func (a *App) AuthMiddleware() azugo.RequestHandlerFunc { return a.authMW }

// Documents returns the document-reference validation client (nil until the
// document service base URL is configured).
func (a *App) Documents() *clients.Documents { return a.documents }

// Audit returns the audit + notification recorder (its channels no-op when
// unconfigured). The routes call it on each transition.
func (a *App) Audit() *audit.Recorder { return a.audit }

// SetAuthMiddleware overrides the inbound auth middleware (test use only).
func (a *App) SetAuthMiddleware(mw azugo.RequestHandlerFunc) { a.authMW = mw }

// SetDocuments injects the document-reference validation client (test use only).
func (a *App) SetDocuments(d *clients.Documents) { a.documents = d }

// SetAudit injects the audit + notification recorder (test use only).
func (a *App) SetAudit(r *audit.Recorder) { a.audit = r }
