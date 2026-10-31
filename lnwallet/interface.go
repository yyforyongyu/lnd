package lnwallet

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/chanvalidate"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// DefaultAccountName is the name for the default account used to manage
	// on-chain funds within the wallet.
	DefaultAccountName = "default"
)

// DefaultPublicPassphrase is the default public wallet passphrase.
var DefaultPublicPassphrase = []byte("public")

// DefaultPrivatePassphrase is the default private wallet passphrase.
var DefaultPrivatePassphrase = []byte("hello")

// ErrNoOutputs rejects transaction requests without recipients.
var ErrNoOutputs = errors.New("no outputs")

// ErrInvalidMinconf rejects a negative confirmation requirement.
var ErrInvalidMinconf = errors.New("minimum number of confirmations must " +
	"be a non-negative number")

// WalletAPI is the maintained wallet borrowed by Lightning and RPC callers.
// Each embedded capability is implemented by the one Manager-owned Wallet;
// this view adds no wallet behavior or separate lifetime.
//
//nolint:interfacebloat // Compose one native Wallet without another adapter.
type WalletAPI interface {
	base.Controller
	base.AccountManager
	base.AddressManager
	base.UtxoManager
	base.TxCreator
	base.TxReader
	base.TxWriter
	base.TxPublisher
	base.PsbtManager
	base.TxSubscriber

	// IsWatchOnly distinguishes external signing from unavailable local
	// funds when Lightning selects coins from the native UTXO snapshot.
	IsWatchOnly() bool

	// DerivePubKey reads an existing child without allocating an address;
	// RPC key listings and read-only fee quotes require this distinction.
	DerivePubKey(context.Context, base.DerivePubKeyParams) (
		*btcec.PublicKey, error)
}

var _ WalletAPI = (*base.Wallet)(nil)

// UtxoMature reports whether a native output is past any coinbase restriction.
// Spendable also encodes local custody, so external-signing and imported funds
// need their parent inspected when confirmations alone cannot prove maturity.
func UtxoMature(ctx context.Context, reader base.TxReader, utxo *base.Utxo,
	coinbaseMaturity uint16) (bool, error) {

	if utxo.Spendable || utxo.Confirmations >= int32(coinbaseMaturity) {
		return true, nil
	}

	// Native Utxo does not carry coinbase origin. Resolve it from the
	// maintained history rather than inferring maturity from key custody.
	parent, err := reader.GetTx(ctx, utxo.OutPoint.Hash)
	if err != nil {
		return false, err
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(parent.RawTx)); err != nil {
		return false, fmt.Errorf("decode UTXO parent: %w", err)
	}

	return !blockchain.IsCoinBaseTx(&tx), nil
}

// WalletServices provides application startup, chain relay and recovery status.
// Manager and chain ownership remain outside Wallet's data/signing interfaces.
type WalletServices interface {
	// Start authenticates the wallet and initializes Lightning accounts.
	Start() error

	// Stop joins Manager work before stopping the borrowed chain source.
	Stop() error

	// ReadySignal waits for local startup or remote signer readiness.
	ReadySignal(context.Context) chan error

	// BackEnd identifies the connected consensus service.
	BackEnd() string

	// SubmitPackage relays a package through the connected chain service.
	SubmitPackage([]*wire.MsgTx, *chainfee.SatPerVByte) (
		*btcjson.SubmitPackageResult, error)

	// GetRecoveryInfo projects startup recovery policy and scan progress.
	GetRecoveryInfo() (bool, float64, error)
}

// BlockChainIO is a dedicated source which will be used to obtain queries
// related to the current state of the blockchain. The data returned by each of
// the defined methods within this interface should always return the most up
// to date data possible.
//
// TODO(roasbeef): move to diff package perhaps?
// TODO(roasbeef): move publish txn here?
type BlockChainIO interface {
	// GetBestBlock returns the current height and block hash of the valid
	// most-work chain the implementation is aware of.
	GetBestBlock() (*chainhash.Hash, int32, error)

	// GetUtxo attempts to return the passed outpoint if it's still a
	// member of the utxo set. The passed height hint should be the "birth
	// height" of the passed outpoint. The script passed should be the
	// script that the outpoint creates. In the case that the output is in
	// the UTXO set, then the output corresponding to that output is
	// returned.  Otherwise, a non-nil error will be returned.
	// As for some backends this call can initiate a rescan, the passed
	// cancel channel can be closed to abort the call.
	GetUtxo(op *wire.OutPoint, pkScript []byte, heightHint uint32,
		cancel <-chan struct{}) (*wire.TxOut, error)

	// GetBlockHash returns the hash of the block in the best blockchain
	// at the given height.
	GetBlockHash(blockHeight int64) (*chainhash.Hash, error)

	// GetBlock returns the block in the main chain identified by the given
	// hash.
	GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error)

	// GetBlockHeader returns the block header for the given block hash.
	GetBlockHeader(blockHash *chainhash.Hash) (*wire.BlockHeader, error)
}

// MessageSigner represents an abstract object capable of signing arbitrary
// messages. The capabilities of this interface are used to sign announcements
// to the network, or just arbitrary messages that leverage the wallet's keys
// to attest to some message.
type MessageSigner interface {
	// SignMessage attempts to sign a target message with the private key
	// described in the key locator. If the target private key is unable to
	// be found, then an error will be returned. The actual digest signed is
	// the single or double SHA-256 of the passed message.
	SignMessage(keyLoc keychain.KeyLocator, msg []byte,
		doubleHash bool) (*ecdsa.Signature, error)
}

// AddrWithKey wraps a normal addr, but also includes the internal key for the
// delivery addr if known.
type AddrWithKey struct {
	lnwire.DeliveryAddress

	InternalKey fn.Option[keychain.KeyDescriptor]

	// TODO(roasbeef): consolidate w/ instance in chan closer
}

// InternalKeyForAddr returns the internal key associated with a taproot
// address.
func InternalKeyForAddr(wallet base.AddressManager, netParams *chaincfg.Params,
	deliveryScript []byte) (fn.Option[keychain.KeyDescriptor], error) {

	none := fn.None[keychain.KeyDescriptor]()

	pkScript, err := txscript.ParsePkScript(deliveryScript)
	if err != nil {
		return none, err
	}
	addr, err := pkScript.Address(netParams)
	if err != nil {
		return none, err
	}

	// If it's not a taproot address, we don't require to know the internal
	// key in the first place. So we don't return an error here, but also no
	// internal key.
	_, isTaproot := addr.(*address.AddressTaproot)
	if !isTaproot {
		return none, nil
	}

	walletAddr, err := wallet.GetAddressInfo(context.Background(), addr)
	if err != nil {
		// If the error is that the address can't be found, it is not
		// an error. This happens when any channel which is not a custom
		// taproot channel is cooperatively closed to an external P2TR
		// address. In this case there is no internal key associated
		// with the address. Callers can use the .Option() method to get
		// an option value.
		if errors.Is(err, base.ErrAddressNotFound) {
			return none, nil
		}

		return none, err
	}

	// Imported addresses do not provide private keys, so they do not
	// implement waddrmgr.ManagedPubKeyAddress. See RPC ImportTapscript.
	if walletAddr.Imported {
		return none, nil
	}

	// Derived taproot outputs must carry an internal key and full origin;
	// imported outputs above intentionally do not promise either value.
	if walletAddr.PubKey == nil || walletAddr.Derivation == nil {
		return none, fmt.Errorf(
			"address has no key derivation metadata",
		)
	}
	derivationPath := walletAddr.Derivation

	return fn.Some[keychain.KeyDescriptor](keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(derivationPath.Account),
			Index:  derivationPath.Index,
		},
		PubKey: walletAddr.PubKey,
	}), nil
}

// FetchFundingTxWrapper is a wrapper around FetchFundingTx, except that it will
// exit when the supplied quit channel is closed.
func FetchFundingTxWrapper(chain BlockChainIO, chanID lnwire.ShortChannelID,
	quit chan struct{}) (*wire.MsgTx, error) {

	txChan := make(chan *wire.MsgTx, 1)
	errChan := make(chan error, 1)

	go func() {
		tx, err := FetchFundingTx(chain, chanID)
		if err != nil {
			errChan <- err
			return
		}

		txChan <- tx
	}()

	select {
	case tx := <-txChan:
		return tx, nil

	case err := <-errChan:
		return nil, err

	case <-quit:
		return nil, fmt.Errorf("quit channel passed to " +
			"lnwallet.FetchFundingTxWrapper has been closed")
	}
}

// FetchFundingTx uses the given BlockChainIO to fetch and return the funding
// transaction identified by the passed short channel ID.
//
// TODO(roasbeef): replace with call to GetBlockTransaction? (would allow to
// later use getblocktxn).
func FetchFundingTx(chain BlockChainIO,
	chanID lnwire.ShortChannelID) (*wire.MsgTx, error) {

	// First fetch the block hash by the block number encoded, then use
	// that hash to fetch the block itself.
	blockNum := int64(chanID.BlockHeight)
	blockHash, err := chain.GetBlockHash(blockNum)
	if err != nil {
		return nil, err
	}

	fundingBlock, err := chain.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	// As a sanity check, ensure that the advertised transaction index is
	// within the bounds of the total number of transactions within a
	// block.
	numTxns := uint32(len(fundingBlock.Transactions))
	if chanID.TxIndex > numTxns-1 {
		return nil, fmt.Errorf("tx_index=#%v "+
			"is out of range (max_index=%v), network_chan_id=%v",
			chanID.TxIndex, numTxns-1, chanID)
	}

	return fundingBlock.Transactions[chanID.TxIndex].Copy(), nil
}

// FetchPKScriptWithQuit fetches the output script for the given SCID and exits
// early with an error if the provided quit channel is closed before
// completion.
func FetchPKScriptWithQuit(chain BlockChainIO, chanID lnwire.ShortChannelID,
	quit chan struct{}) ([]byte, error) {

	tx, err := FetchFundingTxWrapper(chain, chanID, quit)
	if err != nil {
		return nil, err
	}

	outputLocator := chanvalidate.ShortChanIDChanLocator{
		ID: chanID,
	}

	output, _, err := outputLocator.Locate(tx)
	if err != nil {
		return nil, err
	}

	return output.PkScript, nil
}
