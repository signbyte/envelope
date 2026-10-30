package store

import (
	"context"
	"strings"
	"testing"
	"unsafe"

	"github.com/go-quicktest/qt"
)

// An envelope id read from the route points into a buffer the server reuses for
// the next request. The store files an envelope's documents and slots under the
// id it is given, so it must keep a copy of its own, or they come to sit under
// whatever the buffer holds next and drop off their envelope.

func draftAndRouteID(t *testing.T, m *Memory) (string, []byte, string) {
	t.Helper()

	created, err := m.CreateEnvelope(context.Background(), CreateEnvelopeInput{Owner: "user-a", Title: "t", OrderPolicy: "parallel"})
	qt.Assert(t, qt.IsNil(err))

	buf := []byte(created.ID)
	fromTheRequest := unsafe.String(&buf[0], len(buf)) //nolint:gosec // the aliasing a request read makes, on purpose

	return created.ID, buf, fromTheRequest
}

func TestMemoryAttachKeepsItsOwnCopyOfTheEnvelopeID(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, buf, fromTheRequest := draftAndRouteID(t, m)

	qt.Assert(t, qt.IsNil(m.AttachDocument(ctx, fromTheRequest, "user-a", "doc-1", "hash-1")))

	copy(buf, strings.Repeat("x", len(buf))) // the next request reuses the buffer

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(view.Documents), 1))
	qt.Check(t, qt.Equals(view.Documents[0].EnvelopeID, envID))
}

func TestMemoryAddSlotKeepsItsOwnCopyOfTheEnvelopeID(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	envID, buf, fromTheRequest := draftAndRouteID(t, m)

	_, err := m.AddSlot(ctx, AddSlotInput{EnvelopeID: fromTheRequest, Owner: "user-a", OrderIndex: 1})
	qt.Assert(t, qt.IsNil(err))

	copy(buf, strings.Repeat("x", len(buf))) // the next request reuses the buffer

	view, err := m.GetEnvelope(ctx, envID, "user-a", "")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(len(view.Slots), 1))
	qt.Check(t, qt.Equals(view.Slots[0].EnvelopeID, envID))
}
