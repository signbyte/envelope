package envelope

import (
	"strings"
	"time"

	azugocfg "azugo.io/azugo/config"
	corecfg "azugo.io/core/config"
	"azugo.io/core/validation"
	"github.com/spf13/viper"

	"github.com/gmb-lib/go-authbyte/authclient"
	"github.com/gmb-lib/go-gdpr-audit/gdpr"
	pkconfig "github.com/gmb-lib/go-platform-kit/config"
)

// Configuration is the envelope/workflow service configuration: the platform base
// config, the inbound go-authbyte DPoP validation (this service's own audience),
// the document service it validates references against on behalf of the user, the
// outbound service-client identity, and the durable-state DSN (the envelope schema).
type Configuration struct {
	*pkconfig.BaseConfiguration `mapstructure:",squash"`

	// Auth is the inbound DPoP validation config (AUTH_ISSUER_URL /
	// SERVICE_AUDIENCE / ...). Inbound callers are the backend-for-frontend (the
	// owner's actions, delegated) and the Signing Orchestrator (the slot-signed
	// callback), presenting service tokens for this service's audience.
	Auth *authclient.Configuration `mapstructure:"auth"`

	// DocumentBaseURL is the document service this service validates references
	// against; DocumentAudience is the target audience for the delegated token.
	// Empty DocumentBaseURL means document validation is unavailable and attach
	// fails closed.
	DocumentBaseURL  string `mapstructure:"document_base_url" validate:"omitempty,url"`
	DocumentAudience string `mapstructure:"document_audience"`

	// StoreDSN selects + configures the PostgreSQL backend for the envelope schema
	// (reached only through SECURITY DEFINER procedures under an EXECUTE-only role).
	// Empty selects the in-memory backend (development/test).
	StoreDSN string `mapstructure:"envelope_store_dsn"`

	// Outbound service-client identity. The client id/secret authenticate the
	// outbound DPoP service tokens used to call the document service on behalf of
	// the user; OutboundIssuerURL points the token mint at the in-network auth
	// address (the issuer claim stays Auth.IssuerURL).
	ServiceClientID     string `mapstructure:"service_client_id"`
	ServiceClientSecret string `mapstructure:"service_client_secret"`
	OutboundIssuerURL   string `mapstructure:"outbound_issuer_url" validate:"omitempty,url"`

	// GDPR personal-data-access audit. Recipient and signer identities on an
	// envelope are personal data, so the transitions that process them (create,
	// send, decline) are recorded to the access-audit service over an outbound DPoP
	// service token. Empty AccessAuditURL leaves access auditing off (the records
	// no-op) — for the build/boot test, never production.
	AccessAuditURL       string `mapstructure:"access_audit_url" validate:"omitempty,url"`
	AccessAuditAudience  string `mapstructure:"access_audit_audience"`
	AccessAuditScope     string `mapstructure:"access_audit_scope"`
	AccessAuditOutboxDir string `mapstructure:"access_audit_outbox_dir"`

	// EnvelopeEventsTopic is the broker topic the workflow lifecycle events
	// (created/sent/signed/completed/declined/cancelled) publish to, for the
	// notification consumer. Without a broker connection (BROKER_URL on the base
	// configuration) the events go to the development log transport instead.
	EnvelopeEventsTopic string `mapstructure:"envelope_events_topic"`

	// Retention — when an envelope leaves. A finished envelope keeps personal data
	// (signer identities and names, the owner's subject) that has no purpose left to
	// serve once its documents' bytes are gone, so the row is deleted on a clock.
	// Every window is configurable because a deployment's obligations are not this
	// service's to assume.

	// RetentionGrace is how long a finished envelope stays readable AFTER its
	// documents stop being downloadable. The horizon itself is not configured here
	// and never was worth restating: it is read from the service that owns the
	// documents at the moment the envelope goes terminal, so the two can never drift
	// apart. This is only the margin on top of it.
	RetentionGrace time.Duration `mapstructure:"envelope_retention_grace" validate:"gte=0"`
	// DraftTTL is how long a draft nobody ever sent survives before it is deleted as
	// abandoned. Zero leaves abandoned drafts in place.
	DraftTTL time.Duration `mapstructure:"envelope_draft_ttl" validate:"gte=0"`
	// DefaultExpiry is the expiry applied at create when the caller names none — the
	// deadline by which the invited parties are expected to sign. Zero leaves an
	// envelope open-ended, which also keeps it out of the expiring stage of the sweep.
	DefaultExpiry time.Duration `mapstructure:"envelope_default_expiry" validate:"gte=0"`
	// RetentionSweepInterval is how often the background sweep runs.
	RetentionSweepInterval time.Duration `mapstructure:"envelope_retention_sweep_interval" validate:"required,gt=0"`
	// RetentionSweepBatch caps how many rows one sweep pass touches per stage, so a
	// large backlog drains over successive passes rather than one long lock.
	RetentionSweepBatch int `mapstructure:"envelope_retention_sweep_batch" validate:"gte=0"`
}

// NewConfiguration returns the configuration skeleton for binding.
func NewConfiguration() *Configuration {
	return &Configuration{BaseConfiguration: pkconfig.New()}
}

// ServerCore returns the embedded azugo configuration.
func (c *Configuration) ServerCore() *azugocfg.Configuration {
	return c.Configuration
}

// Bind registers defaults and environment bindings.
func (c *Configuration) Bind(_ string, v *viper.Viper) {
	c.BaseConfiguration.Bind("", v)
	c.Auth = azugocfg.Bind(c.Auth, "auth", v)

	// Dev-only user-token concession (off by default).

	// Document service (reference validation on behalf of the user).
	v.SetDefault("document_audience", "svc:document")
	_ = v.BindEnv("document_base_url", "DOCUMENT_BASE_URL")
	_ = v.BindEnv("document_audience", "DOCUMENT_AUDIENCE")

	// Durable state.
	_ = v.BindEnv("envelope_store_dsn", "ENVELOPE_STORE_DSN")

	// Outbound service-client identity.
	v.SetDefault("service_client_id", "svc:envelope")
	loadSecret(v, "service_client_secret", "SERVICE_CLIENT_SECRET")
	_ = v.BindEnv("service_client_id", "SERVICE_CLIENT_ID")
	_ = v.BindEnv("service_client_secret", "SERVICE_CLIENT_SECRET")
	_ = v.BindEnv("outbound_issuer_url", "OUTBOUND_ISSUER_URL")

	// GDPR access audit — off until ACCESS_AUDIT_URL is set.
	v.SetDefault("access_audit_audience", "svc:access-audit")
	v.SetDefault("access_audit_scope", "access-audit:write")
	_ = v.BindEnv("access_audit_url", "ACCESS_AUDIT_URL")
	_ = v.BindEnv("access_audit_audience", "ACCESS_AUDIT_AUDIENCE")
	_ = v.BindEnv("access_audit_scope", "ACCESS_AUDIT_SCOPE")
	_ = v.BindEnv("access_audit_outbox_dir", "ACCESS_AUDIT_OUTBOX_DIR")

	// Workflow lifecycle events.
	v.SetDefault("envelope_events_topic", "envelope.events")
	_ = v.BindEnv("envelope_events_topic", "ENVELOPE_EVENTS_TOPIC")

	// Retention. The defaults are the shape a public signing portal wants: a week to
	// sign, a couple of days of margin after the documents stop being downloadable,
	// and a month before an untouched draft is treated as abandoned. A deployment
	// with different obligations sets its own.
	v.SetDefault("envelope_retention_grace", 48*time.Hour)
	v.SetDefault("envelope_draft_ttl", 720*time.Hour)
	v.SetDefault("envelope_default_expiry", 168*time.Hour)
	v.SetDefault("envelope_retention_sweep_interval", time.Hour)
	v.SetDefault("envelope_retention_sweep_batch", 500)
	_ = v.BindEnv("envelope_retention_grace", "ENVELOPE_RETENTION_GRACE")
	_ = v.BindEnv("envelope_draft_ttl", "ENVELOPE_DRAFT_TTL")
	_ = v.BindEnv("envelope_default_expiry", "ENVELOPE_DEFAULT_EXPIRY")
	_ = v.BindEnv("envelope_retention_sweep_interval", "ENVELOPE_RETENTION_SWEEP_INTERVAL")
	_ = v.BindEnv("envelope_retention_sweep_batch", "ENVELOPE_RETENTION_SWEEP_BATCH")
}

// Validate validates the full configuration tree.
func (c *Configuration) Validate(valid *validation.Validate) error {
	if err := c.BaseConfiguration.Validate(valid); err != nil {
		return err
	}
	if err := c.Auth.Validate(valid); err != nil {
		return err
	}

	return valid.Struct(c)
}

// StorePostgres reports whether a Postgres DSN is configured (else in-memory).
func (c *Configuration) StorePostgres() bool {
	return strings.TrimSpace(c.StoreDSN) != ""
}

// DocumentEnabled reports whether the document service is configured, so the
// document client is worth building. Without it, attaching a document reference
// fails closed.
func (c *Configuration) DocumentEnabled() bool {
	return strings.TrimSpace(c.DocumentBaseURL) != ""
}

// AccessAuditEnabled reports whether GDPR access auditing is wired.
func (c *Configuration) AccessAuditEnabled() bool {
	return strings.TrimSpace(c.AccessAuditURL) != ""
}

// OutboundEnabled reports whether any outbound DPoP service call is configured —
// the document reference validation (on behalf of the user) or the access-audit
// poster — so the single outbound auth client is worth building (both reuse it;
// audience + scope are chosen per call).
func (c *Configuration) OutboundEnabled() bool {
	return c.DocumentEnabled() || c.AccessAuditEnabled()
}

// GDPRConfig builds the go-gdpr-audit client configuration from the access-audit
// settings, with the library's default resilience knobs.
func (c *Configuration) GDPRConfig() gdpr.Configuration {
	return gdpr.Configuration{
		Endpoint:         c.AccessAuditURL,
		Audience:         c.AccessAuditAudience,
		Scope:            c.AccessAuditScope,
		Timeout:          gdpr.DefaultTimeout,
		OutboxCapacity:   gdpr.DefaultOutboxCapacity,
		MaxRetries:       gdpr.DefaultMaxRetries,
		RetryBackoff:     gdpr.DefaultRetryBackoff,
		BreakerThreshold: gdpr.DefaultBreakerThreshold,
		BreakerCooldown:  gdpr.DefaultBreakerCooldown,
	}
}

// outboundIssuer returns the issuer base for the outbound service-token mint.
func (c *Configuration) outboundIssuer() string {
	if u := strings.TrimSpace(c.OutboundIssuerURL); u != "" {
		return u
	}

	return c.Auth.IssuerURL
}

// OutboundAuthClientConfig builds the outbound auth-client config: it reuses the
// validated inbound Auth settings and adds this service's client credentials + the
// optional issuer override.
func (c *Configuration) OutboundAuthClientConfig() *authclient.Configuration {
	cfg := *c.Auth // copy the validated inbound config
	cfg.IssuerURL = c.outboundIssuer()
	cfg.ServiceClientID = c.ServiceClientID
	cfg.ServiceClientSecret = c.ServiceClientSecret

	return &cfg
}

// loadSecret resolves a secret from the secret store (Vault agent -> <NAME>_FILE)
// and registers it as a default so an explicit env value still overrides it.
func loadSecret(v *viper.Viper, key, name string) {
	if secret, err := corecfg.LoadRemoteSecret(name); err == nil && secret != "" {
		v.SetDefault(key, secret)
	}
}
