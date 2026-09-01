package audit

import (
	"context"
	"time"

	"azugo.io/core"

	"github.com/gmb-lib/go-gdpr-audit/gdpr"
)

// drainTask runs the access-audit client's background outbox delivery as a
// core.Tasker, so buffered access records deliver in the background and flush on
// shutdown without an App.Start/Stop override.
type drainTask struct {
	client *gdpr.Client
}

// NewDrainTask returns a Tasker that drains buffered access records and flushes
// them on shutdown.
func NewDrainTask(client *gdpr.Client) core.Tasker {
	return &drainTask{client: client}
}

func (t *drainTask) Name() string { return "gdpr-audit-drain" }

func (t *drainTask) Start(ctx context.Context) error {
	go t.client.Drain(ctx)

	return nil
}

func (t *drainTask) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = t.client.Close(ctx)
}
