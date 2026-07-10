package dyn

import (
	"context"
	"sync"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// MemPersister is an in-memory Persister. It is primarily intended for tests
// and as the reference implementation of the Persister contract; the durable
// channeldb-backed implementation lands in the reestablish branch.
type MemPersister struct {
	mu    sync.Mutex
	store map[lnwire.ChannelID]AcceptedProposal
}

// A compile-time assertion that MemPersister implements Persister.
var _ Persister = (*MemPersister)(nil)

// NewMemPersister constructs an empty in-memory Persister.
func NewMemPersister() *MemPersister {
	return &MemPersister{
		store: make(map[lnwire.ChannelID]AcceptedProposal),
	}
}

// StoreAcceptedProposal persists (or overwrites) the accepted-proposal context
// for the given channel.
//
// NOTE: Part of the Persister interface.
func (m *MemPersister) StoreAcceptedProposal(_ context.Context,
	chanID lnwire.ChannelID, p AcceptedProposal) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	m.store[chanID] = p

	return nil
}

// FetchAcceptedProposal loads the persisted context for the given channel, if
// any.
//
// NOTE: Part of the Persister interface.
func (m *MemPersister) FetchAcceptedProposal(_ context.Context,
	chanID lnwire.ChannelID) (fn.Option[AcceptedProposal], error) {

	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.store[chanID]
	if !ok {
		return fn.None[AcceptedProposal](), nil
	}

	return fn.Some(p), nil
}

// DeleteAcceptedProposal removes any persisted context for the given channel.
//
// NOTE: Part of the Persister interface.
func (m *MemPersister) DeleteAcceptedProposal(_ context.Context,
	chanID lnwire.ChannelID) error {

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.store, chanID)

	return nil
}
