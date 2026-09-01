package envelope

import (
	"context"

	"go.uber.org/zap"

	"github.com/gmb-lib/go-platform-kit/broker"
)

// logTransport is a broker transport that writes published events to the service
// logger. It lets the workflow-event publisher emit without a broker connection in
// development; the platform transport is service-provided, so a real broker client
// is injected in production with no code change here. Redaction still applies (the
// platform installs it once) and the broker envelope strips token-shaped attributes.
type logTransport struct{ log *zap.Logger }

// newLogTransport returns a logging broker transport.
func newLogTransport(log *zap.Logger) broker.Transport {
	if log == nil {
		log = zap.NewNop()
	}

	return &logTransport{log: log}
}

// Publish writes the event payload to the logger as an envelope_event line.
func (t *logTransport) Publish(_ context.Context, topic, key string, payload []byte) error {
	t.log.Info("envelope_event",
		zap.String("topic", topic),
		zap.String("key", key),
		zap.ByteString("event", payload),
	)

	return nil
}
