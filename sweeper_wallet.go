package lnd

import (
	"github.com/btcsuite/btcd/chainhash/v2"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// sweeperWallet is a wrapper around the LightningWallet that implements the
// sweeper's Wallet interface.
type sweeperWallet struct {
	*lnwallet.LightningWallet

	// Borrowed native views provide storage/relay capabilities directly;
	// LightningWallet retains channel-specific selection and
	// rebroadcasting.
	base.TxReader
	base.TxWriter
	base.TxPublisher
	lnwallet.WalletServices
}

// newSweeperWallet creates a new sweeper wallet from the given
// LightningWallet.
func newSweeperWallet(w *lnwallet.LightningWallet) *sweeperWallet {
	return &sweeperWallet{
		LightningWallet: w,
		TxReader:        w.Cfg.Wallet,
		TxWriter:        w.Cfg.Wallet,
		TxPublisher:     w.Cfg.Wallet,
		WalletServices:  w.Cfg.WalletServices,
	}
}

// CancelRebroadcast cancels the rebroadcast of the given transaction.
func (s *sweeperWallet) CancelRebroadcast(txid chainhash.Hash) {
	// For neutrino, we don't config the rebroadcaster for the wallet as it
	// manages the rebroadcasting logic in neutrino itself.
	if s.Cfg.Rebroadcaster != nil {
		s.Cfg.Rebroadcaster.MarkAsConfirmed(txid)
	}
}
