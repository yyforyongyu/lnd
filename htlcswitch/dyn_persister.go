package htlcswitch

import (
	"context"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch/dyn"
	"github.com/lightningnetwork/lnd/lnwire"
)

// dbDynPersister is the durable, channeldb-backed implementation of the
// dyn.Persister interface. It stores the accepted-proposal context per channel
// so an in-flight dynamic-commitments negotiation survives a restart and can be
// resumed or forgotten on reconnect. It converts between the htlcswitch/dyn
// context type and the channeldb-native storage type, keeping channeldb free of
// any dependency on htlcswitch/dyn.
type dbDynPersister struct {
	// db is the channel-state database the context is persisted to.
	db *channeldb.ChannelStateDB
}

// A compile-time assertion that dbDynPersister implements dyn.Persister.
var _ dyn.Persister = (*dbDynPersister)(nil)

// newDBDynPersister constructs a channeldb-backed dyn.Persister over the given
// channel-state database.
func newDBDynPersister(db *channeldb.ChannelStateDB) *dbDynPersister {
	return &dbDynPersister{db: db}
}

// StoreAcceptedProposal persists (or overwrites) the accepted-proposal context
// for the given channel.
//
// NOTE: Part of the dyn.Persister interface.
func (p *dbDynPersister) StoreAcceptedProposal(_ context.Context,
	chanID lnwire.ChannelID, prop dyn.AcceptedProposal) error {

	return p.db.StoreDynAcceptedProposal(chanID, channeldb.DynAcceptedProposal{
		Proposer:         prop.Proposer,
		Proposal:         prop.Proposal,
		NextCommitHeight: prop.NextCommitHeight,
		AckSig:           prop.AckSig,
		CommitSig:        prop.CommitSig,
	})
}

// FetchAcceptedProposal loads the persisted context for the given channel, if
// any.
//
// NOTE: Part of the dyn.Persister interface.
func (p *dbDynPersister) FetchAcceptedProposal(_ context.Context,
	chanID lnwire.ChannelID) (fn.Option[dyn.AcceptedProposal], error) {

	stored, err := p.db.FetchDynAcceptedProposal(chanID)
	if err != nil {
		return fn.None[dyn.AcceptedProposal](), err
	}

	return fn.MapOption(func(s channeldb.DynAcceptedProposal,
	) dyn.AcceptedProposal {

		return dyn.AcceptedProposal{
			Proposer:         s.Proposer,
			Proposal:         s.Proposal,
			NextCommitHeight: s.NextCommitHeight,
			AckSig:           s.AckSig,
			CommitSig:        s.CommitSig,
		}
	})(stored), nil
}

// DeleteAcceptedProposal removes any persisted context for the given channel.
//
// NOTE: Part of the dyn.Persister interface.
func (p *dbDynPersister) DeleteAcceptedProposal(_ context.Context,
	chanID lnwire.ChannelID) error {

	return p.db.DeleteDynAcceptedProposal(chanID)
}
