package btcwallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/btcutil/v2/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/pkg/btcunit"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	defaultAccount  = uint32(waddrmgr.DefaultAccountNum)
	importedAccount = uint32(waddrmgr.ImportedAddrAccount)

	// dryRunImportAccountNumAddrs represents the number of addresses we'll
	// derive for an imported account's external and internal branch when a
	// dry run is attempted.
	dryRunImportAccountNumAddrs = 5

	// UnconfirmedHeight is the special case end height that is used to
	// obtain unconfirmed transactions from ListTransactionDetails.
	UnconfirmedHeight int32 = -1
)

var (
	// lightningAddrSchema is the scope addr schema for all keys that we
	// derive. We'll treat them all as p2wkh addresses, as atm we must
	// specify a particular type.
	lightningAddrSchema = waddrmgr.ScopeAddrSchema{
		ExternalAddrType: waddrmgr.WitnessPubKey,
		InternalAddrType: waddrmgr.WitnessPubKey,
	}

	// LndDefaultKeyScopes is the list of default key scopes that lnd adds
	// to its wallet.
	LndDefaultKeyScopes = []waddrmgr.KeyScope{
		waddrmgr.KeyScopeBIP0049Plus,
		waddrmgr.KeyScopeBIP0084,
		waddrmgr.KeyScopeBIP0086,
	}

	// errNoImportedAddrGen is an error returned when a new address is
	// requested for the default imported account within the wallet.
	errNoImportedAddrGen = errors.New("addresses cannot be generated for " +
		"the default imported account")
)

// walletAPI composes the maintained wallet capabilities used by this adapter.
// Embedding the existing interfaces keeps mock expectations at the same
// boundary without a second runtime or a forwarding implementation.
type walletAPI interface {
	base.Controller
	base.AccountManager
	base.AddressManager
	base.UnsafeSigner
	base.UtxoManager
	base.TxCreator
	base.TxPublisher
	base.TxReader
	base.TxWriter
	base.PsbtManager
	base.TxSubscriber
}

var _ walletAPI = (*base.Wallet)(nil)

// BtcWallet is an implementation of the lnwallet.WalletController interface
// backed by an active instance of btcwallet. At the time of the writing of
// this documentation, this implementation requires a full btcd node to
// operate.
type BtcWallet struct {
	// wallet is an active instance of btcwallet.
	wallet walletAPI

	chain chain.Interface

	cfg *Config

	netParams *chaincfg.Params

	chainKeyScope waddrmgr.KeyScope

	// accountMtx serialises the calls that add a named account, meaning
	// CreateAccount and ImportAccount. Both do the same check-then-act
	// against the same invariant — a name must exist in at most one key
	// scope — and both need two database transactions to do it, since
	// btcwallet exposes no way to check and create in one. Without a lock
	// covering both, two concurrent calls (in either combination) can pass
	// their duplicate checks and then each create the name under a
	// different scope, which is precisely the ambiguity the checks exist
	// to prevent.
	accountMtx sync.Mutex

	blockCache *blockcache.BlockCache

	*input.MusigSessionManager
}

// A compile time check to ensure that BtcWallet implements the
// WalletController and BlockChainIO interfaces.
var _ lnwallet.WalletController = (*BtcWallet)(nil)
var _ lnwallet.BlockChainIO = (*BtcWallet)(nil)

// New returns a new fully initialized instance of BtcWallet given a valid
// configuration struct.
func New(cfg Config, blockCache *blockcache.BlockCache) (*BtcWallet, error) {
	// Create the key scope for the coin type being managed by this wallet.
	chainKeyScope := waddrmgr.KeyScope{
		Purpose: keychain.BIP0043Purpose,
		Coin:    cfg.CoinType,
	}

	// RPC startup hands over the one Manager and its already started
	// wallet. Direct callers own the same sequence here, including chain
	// startup.
	managedWallet := cfg.Wallet
	if managedWallet == nil && cfg.Manager == nil {
		err := cfg.ChainSource.Start(context.Background())
		if err != nil {
			return nil, err
		}
		managerConfig := cfg.ManagerConfig
		managerConfig.ChainParams = *cfg.NetParams
		managerConfig.ChainSource = cfg.ChainSource
		managerConfig.RecoveryWindow = cfg.RecoveryWindow
		managerConfig.KVDBPubPassphrase = cfg.PublicPass
		if cfg.PublicPass == nil {
			managerConfig.KVDBPubPassphrase = defaultPubPassphrase
		}
		manager, err := base.NewManager(
			context.Background(), managerConfig,
		)
		if err != nil {
			cfg.ChainSource.Stop()
			return nil, err
		}
		cfg.Manager = manager
		wallets, err := manager.Start(context.Background())
		if err != nil || len(wallets) > 1 {
			_ = manager.Stop()
			cfg.ChainSource.Stop()
			if err != nil {
				return nil, err
			}

			return nil, fmt.Errorf("lnd requires a single wallet")
		}
		if len(wallets) == 1 {
			managedWallet = wallets[0]
		}
	}

	// Create starts its returned wallet itself. No per-wallet Start or
	// account loading is needed after this Manager-owned operation.
	if managedWallet == nil {
		params := base.CreateWalletParams{
			Name:              "lnd",
			Mode:              base.ModeGenSeed,
			Birthday:          cfg.Birthday,
			PubPassphrase:     cfg.PublicPass,
			PrivatePassphrase: cfg.PrivatePass,
		}
		if cfg.PublicPass == nil {
			params.PubPassphrase = defaultPubPassphrase
		}
		if cfg.HdSeed != nil {
			params.Mode = base.ModeImportSeed
			params.Seed = cfg.HdSeed
		}
		var err error
		managedWallet, err = cfg.Manager.Create(params)
		if err != nil {
			_ = cfg.Manager.Stop()
			cfg.ChainSource.Stop()
			return nil, err
		}
	}

	cfg.Wallet = managedWallet
	finalWallet := &BtcWallet{
		cfg:           &cfg,
		wallet:        managedWallet,
		chain:         cfg.ChainSource,
		netParams:     cfg.NetParams,
		chainKeyScope: chainKeyScope,
		blockCache:    blockCache,
	}

	finalWallet.MusigSessionManager = input.NewMusigSessionManager(
		finalWallet.fetchPrivKey,
	)

	return finalWallet, nil
}

// BackEnd returns the underlying ChainService's name as a string.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) BackEnd() string {
	if b.chain != nil {
		return b.chain.BackEnd()
	}

	return ""
}

// InternalWallet returns a pointer to the internal base wallet which is the
// core of btcwallet.
func (b *BtcWallet) InternalWallet() *base.Wallet {
	return b.cfg.Wallet
}

// Start initializes the underlying rpc connection, the wallet itself, and
// begins syncing to the current available blockchain state.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) Start() error {
	ctx := context.Background()
	walletIsWatchOnly := b.cfg.Wallet.IsWatchOnly()
	if walletIsWatchOnly && !b.cfg.WatchOnly {
		return fmt.Errorf("wallet is watch-only but " +
			"remote signing is disabled")
	}
	if !walletIsWatchOnly && b.cfg.WatchOnly && b.cfg.MigrateWatchOnly {
		return fmt.Errorf("managed wallet does not support " +
			"watch-only conversion")
	}
	// A remote signer may be enabled without purging local keys. Keep the
	// existing warning so the operator sees that this wallet retains them.
	if !walletIsWatchOnly && b.cfg.WatchOnly && !b.cfg.MigrateWatchOnly {
		log.Warnf("Wallet is expected to be in watch-only mode but " +
			"still contains private keys")
	}

	// A successful unlocker handoff already authenticated the wallet.
	// Direct startup authenticates here; Info avoids another expensive
	// unlock and never turns an already-unlocked error into password
	// validation.
	info, err := b.wallet.Info(ctx)
	if err != nil {
		return err
	}
	if info.Locked && !walletIsWatchOnly {
		err = b.wallet.Unlock(ctx, base.UnlockRequest{
			Passphrase: b.cfg.PrivatePass,
			Timeout:    -1,
		})
		if err != nil {
			return err
		}
	}

	// Create missing canonical account zero through the account API. Reopen
	// leaves existing names and allocation cursors intact; account creation
	// does not restart Manager or claim to perform historical recovery.
	for _, scope := range LndDefaultKeyScopes {
		accounts, err := b.wallet.ListAccountsByScope(ctx, scope)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(accounts, func(a base.AccountInfo) bool {
			return a.AccountNumber != nil && *a.AccountNumber == 0
		}) {

			continue
		}
		zero := base.AccountNumber(0)
		schema := waddrmgr.ScopeAddrMap[scope]
		_, err = b.wallet.NewAccount(ctx, base.NewAccountParams{
			Scope:         scope,
			Name:          lnwallet.DefaultAccountName,
			AccountNumber: &zero,
			AddrSchema:    &schema,
		})
		if err != nil {
			return err
		}
	}

	// Purpose 1017 keys sign off-chain contracts, so exclude newly created
	// families from automatic chain scans. Persisted families are reused
	// even when an earlier initialization stopped partway through this
	// loop.
	accounts, err := b.wallet.ListAccountsByScope(ctx, b.chainKeyScope)
	if err != nil {
		return err
	}
	for family := base.AccountNumber(0); family <= 255; family++ {
		if slices.ContainsFunc(accounts, func(a base.AccountInfo) bool {
			return a.AccountNumber != nil &&
				*a.AccountNumber == family
		}) {

			continue
		}
		name := fmt.Sprintf("act:%d", family)
		if family == 0 {
			name = lnwallet.DefaultAccountName
		}
		_, err := b.wallet.NewAccount(ctx, base.NewAccountParams{
			Scope:         b.chainKeyScope,
			Name:          name,
			AccountNumber: &family,
			AddrSchema:    &lightningAddrSchema,
			NoChainSync:   true,
		})
		if err != nil {
			return err
		}
	}
	// PoC: newly installed accounts can be scanned only after the
	// initially started wallet reaches a state that admits Resync.
	if b.cfg.RecoveryWindow > 0 {
		deadline := time.Now().Add(20 * time.Second)
		for {
			info, err = b.wallet.Info(ctx)
			if err != nil {
				return err
			}
			if info.Synced {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("wallet did not admit recovery resync")
			}
			time.Sleep(50 * time.Millisecond)
		}
		return b.wallet.Resync(ctx, uint32(info.BirthdayBlock.Height))
	}

	return nil
}

// Stop joins the Manager's wallet work and closes its database before stopping
// the borrowed chain source, which remains usable throughout wallet shutdown.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) Stop() error {
	err := b.cfg.Manager.Stop()
	b.chain.Stop()
	return err
}

// ReadySignal currently signals that the wallet is ready instantly.
func (b *BtcWallet) ReadySignal(_ context.Context) chan error {
	readyChan := make(chan error, 1)
	readyChan <- nil

	return readyChan
}

// ConfirmedBalance returns the sum of all the wallet's unspent outputs that
// have at least confs confirmations. If confs is set to zero, then all unspent
// outputs, including those currently in the mempool will be included in the
// final sum. The account parameter serves as a filter to retrieve the balance
// for a specific account. When empty, the confirmed balance of all wallet
// accounts is returned.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ConfirmedBalance(confs int32,
	accountFilter string) (btcutil.Amount, error) {

	var balance btcutil.Amount

	witnessOutputs, err := b.ListUnspentWitness(
		confs, math.MaxInt32, accountFilter,
	)
	if err != nil {
		return 0, err
	}

	for _, witnessOutput := range witnessOutputs {
		balance += witnessOutput.Value
	}

	return balance, nil
}

// keyScopeForAccountAddr determines the appropriate key scope of an account
// based on its name/address type.
func (b *BtcWallet) keyScopeForAccountAddr(accountName string,
	addrType lnwallet.AddressType) (waddrmgr.KeyScope, uint32, error) {

	// Map the requested address type to its key scope.
	var addrKeyScope waddrmgr.KeyScope
	switch addrType {
	case lnwallet.WitnessPubKey:
		addrKeyScope = waddrmgr.KeyScopeBIP0084
	case lnwallet.NestedWitnessPubKey:
		addrKeyScope = waddrmgr.KeyScopeBIP0049Plus
	case lnwallet.TaprootPubkey:
		addrKeyScope = waddrmgr.KeyScopeBIP0086
	default:
		return waddrmgr.KeyScope{}, 0,
			fmt.Errorf("unknown address type")
	}

	// The default account spans across multiple key scopes, so the
	// requested address type should already be valid for this account.
	if accountName == lnwallet.DefaultAccountName {
		return addrKeyScope, defaultAccount, nil
	}

	// Otherwise, look up the custom account and if it supports the given
	// key scope.
	accountInfo, err := b.wallet.GetAccount(
		context.Background(), addrKeyScope, accountName,
	)
	if err != nil {
		// A custom account lives in exactly one key scope — one of
		// BIP-0049Plus, BIP-0084 or BIP-0086, fixed when it was
		// created — so asking for an address type that maps elsewhere
		// reports the account as missing even though it exists. That
		// bare "not found" says nothing about which scope to ask for,
		// so check whether the name resolves anywhere before passing
		// it on.
		if errors.Is(err, base.ErrAccountNotFound) {
			scope, _, lookupErr := b.lookupFirstCustomAccount(
				accountName,
			)
			if lookupErr == nil {
				return waddrmgr.KeyScope{}, 0, fmt.Errorf(
					"account %v exists under key scope "+
						"%v, not %v; request the "+
						"address type belonging to "+
						"that scope instead",
					accountName, scope, addrKeyScope)
			}
		}

		return waddrmgr.KeyScope{}, 0, err
	}

	// Named imported accounts need no fabricated BIP32 account number. The
	// result is used only for scope validation by address callers.
	if accountInfo.AccountNumber == nil {
		return addrKeyScope, importedAccount, nil
	}

	return addrKeyScope, uint32(*accountInfo.AccountNumber), nil
}

// NewAddress returns the next external or internal address for the wallet
// dictated by the value of the `change` parameter. If change is true, then an
// internal address will be returned, otherwise an external address should be
// returned. The account parameter must be non-empty as it determines which
// account the address should be generated from.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) NewAddress(t lnwallet.AddressType, change bool,
	accountName string) (address.Address, error) {

	// Addresses cannot be derived from the catch-all imported accounts.
	if accountName == waddrmgr.ImportedAddrAccountName {
		return nil, errNoImportedAddrGen
	}

	keyScope, _, err := b.keyScopeForAccountAddr(accountName, t)
	if err != nil {
		return nil, err
	}

	// The maintained boundary already separates forced allocation from
	// unused-address lookup; retain that distinction at the lnd interface.
	addrType, err := waddrmgr.AddressTypeForScope(keyScope)
	if err != nil {
		return nil, err
	}

	return b.wallet.NewAddress(
		context.Background(),
		accountName,
		addrType,
		change,
	)
}

// LastUnusedAddress returns the last *unused* address known by the wallet. An
// address is unused if it hasn't received any payments. This can be useful in
// UIs in order to continually show the "freshest" address without having to
// worry about "address inflation" caused by continual refreshing. Similar to
// NewAddress it can derive a specified address type, and also optionally a
// change address. The account parameter must be non-empty as it determines
// which account the address should be generated from.
func (b *BtcWallet) LastUnusedAddress(addrType lnwallet.AddressType,
	accountName string) (address.Address, error) {

	// Addresses cannot be derived from the catch-all imported accounts.
	if accountName == waddrmgr.ImportedAddrAccountName {
		return nil, errNoImportedAddrGen
	}

	keyScope, _, err := b.keyScopeForAccountAddr(accountName, addrType)
	if err != nil {
		return nil, err
	}

	walletAddrType, err := waddrmgr.AddressTypeForScope(keyScope)
	if err != nil {
		return nil, err
	}

	return b.wallet.GetUnusedAddress(
		context.Background(), accountName, walletAddrType, false,
	)
}

// IsOurAddress checks if the passed address belongs to this wallet
//
// This is a part of the WalletController interface.
func (b *BtcWallet) IsOurAddress(a address.Address) bool {
	// Ownership comes from stored address metadata, including imported
	// scripts that do not carry a public key or derivation path.
	_, err := b.wallet.GetAddressInfo(context.Background(), a)
	return err == nil
}

// AddressInfo returns the information about an address, if it's known to this
// wallet.
//
// NOTE: This is a part of the WalletController interface.
func (b *BtcWallet) AddressInfo(a address.Address) (*base.AddressInfo,
	error) {

	info, err := b.wallet.GetAddressInfo(context.Background(), a)
	if err != nil {
		return nil, err
	}

	return &info, nil
}

// ListAccounts retrieves all accounts belonging to the wallet by default. A
// name and key scope filter can be provided to filter through all of the wallet
// accounts and return only those matching.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListAccounts(name string,
	keyScope *waddrmgr.KeyScope) ([]*waddrmgr.AccountProperties, error) {

	// Query once and apply lnd's scope visibility locally. This retains
	// purpose-1017 accounts in the unfiltered view and excludes BIP44.
	accounts, err := b.wallet.ListAccounts(context.Background())
	if err != nil {
		return nil, err
	}
	var result []*waddrmgr.AccountProperties
	for _, account := range accounts {
		if name != "" && account.AccountName != name {
			continue
		}
		// Named default/imported views historically cover payment
		// accounts; purpose-1017 families appear only in the unfiltered
		// account view.
		if name != "" && keyScope == nil &&
			account.KeyScope == b.chainKeyScope {

			continue
		}
		if keyScope != nil && account.KeyScope != *keyScope {
			continue
		}
		if keyScope == nil &&
			!slices.Contains(
				LndDefaultKeyScopes, account.KeyScope,
			) &&
			account.KeyScope != b.chainKeyScope {

			continue
		}
		props, err := accountProperties(account)
		if err != nil {
			return nil, err
		}
		result = append(result, props)
	}
	if name != "" && len(result) == 0 {
		return nil, newAccountNotFoundError(name)
	}

	return result, nil
}

// accountProperties retains lnd's existing account result shape. Numberless
// imports use the imported sentinel only in its legacy internal-number field;
// address and signing operations always select them by name, never this value.
func accountProperties(info base.AccountInfo) (
	*waddrmgr.AccountProperties, error) {

	props := &waddrmgr.AccountProperties{
		AccountNumber:    importedAccount,
		AccountName:      info.AccountName,
		ExternalKeyCount: info.ExternalKeyCount,
		InternalKeyCount: info.InternalKeyCount,
		ImportedKeyCount: info.ImportedKeyCount,
		KeyScope:         info.KeyScope,
		IsWatchOnly:      info.IsWatchOnly,
		AddrSchema:       &info.AddrSchema,
	}
	if info.AccountNumber != nil {
		props.AccountNumber = uint32(*info.AccountNumber)
	}
	if info.MasterKeyFingerprint != nil {
		props.MasterKeyFingerprint = uint32(*info.MasterKeyFingerprint)
	}
	if len(info.PublicKey) != 0 {
		key, err := hdkeychain.NewKeyFromString(string(info.PublicKey))
		if err != nil {
			return nil, err
		}
		props.AccountPubKey = key
	}

	return props, nil
}

// newAccountNotFoundError returns an error indicating that the manager didn't
// find the specific account. This error is used to be compatible with the old
// 'LookupAccount' behaviour previously used.
func newAccountNotFoundError(name string) error {
	str := fmt.Sprintf("account name '%s' not found", name)

	return waddrmgr.ManagerError{
		ErrorCode:   waddrmgr.ErrAccountNotFound,
		Description: str,
	}
}

// RequiredReserve returns the minimum amount of satoshis that should be
// kept in the wallet in order to fee bump anchor channels if necessary.
// The value scales with the number of public anchor channels but is
// capped at a maximum.
func (b *BtcWallet) RequiredReserve(
	numAnchorChans uint32) btcutil.Amount {

	anchorChanReservedValue := lnwallet.AnchorChanReservedValue
	reserved := btcutil.Amount(numAnchorChans) * anchorChanReservedValue
	if reserved > lnwallet.MaxAnchorChanReservedValue {
		reserved = lnwallet.MaxAnchorChanReservedValue
	}

	return reserved
}

// ListAddresses retrieves all the addresses along with their balance. An
// account name filter can be provided to filter through all of the
// wallet accounts and return the addresses of only those matching.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListAddresses(name string,
	showCustomAccounts bool) (lnwallet.AccountAddressMap, error) {

	accounts, err := b.ListAccounts(name, nil)
	if err != nil {
		return nil, err
	}

	// Address balances are supplied by the maintained reader; enrich each
	// result with stored public-key metadata for the existing WalletKit
	// view.
	addresses := make(lnwallet.AccountAddressMap)
	for _, account := range accounts {
		var properties []base.AddressProperty
		if account.KeyScope.Purpose == keychain.BIP0043Purpose {
			if !showCustomAccounts {
				continue
			}
			// Custom keys have no on-chain balance. Account cursors
			// bound their allocated children, and public derivation
			// reads each child without consuming a new index or
			// installing a watch.
			selector := base.NewAccountSelectorByNumber(
				account.KeyScope,
				base.AccountNumber(account.AccountNumber),
			)
			addrType := waddrmgr.WitnessPubKey
			for branch, count := range []uint32{
				account.ExternalKeyCount,
				account.InternalKeyCount,
			} {
				for index := uint32(0); index < count; index++ {
					path := base.DerivePubKeyParams{
						Account: selector,
						Branch:  uint32(branch),
						Index:   index,
					}
					pubKey, err := b.wallet.DerivePubKey(
						context.Background(), path,
					)
					if err != nil {
						return nil, err
					}
					keyBytes := pubKey.SerializeCompressed()
					addr, err :=
						addrType.AddrFromPubKeyBytes(
							keyBytes, b.netParams,
						)
					if err != nil {
						return nil, err
					}
					property := base.AddressProperty{
						Address: addr,
					}
					properties = append(
						properties,
						property,
					)
				}
			}
		} else {
			addrType, err := waddrmgr.AddressTypeForScope(
				account.KeyScope,
			)
			if err != nil {
				return nil, err
			}
			properties, err = b.wallet.ListAddresses(
				context.Background(),
				account.AccountName, addrType,
			)
			if err != nil {
				return nil, err
			}
		}
		for _, property := range properties {
			info, err := b.wallet.GetAddressInfo(
				context.Background(), property.Address,
			)
			if err != nil {
				return nil, err
			}
			// The imported alias must exclude derived addresses
			// even when a backend enumerates the whole wallet for
			// that alias.
			if account.AccountName ==
				waddrmgr.ImportedAddrAccountName &&
				!info.Imported {

				continue
			}
			_, _, path, _ := Bip32DerivationFromAddress(&info)
			addressString := property.Address.String()
			if account.KeyScope.Purpose == keychain.BIP0043Purpose {
				keyBytes := info.PubKey.SerializeCompressed()
				addressString = hex.EncodeToString(keyBytes)
			}
			addresses[account] = append(
				addresses[account], lnwallet.AddressProperty{
					Address:        addressString,
					Internal:       info.Internal,
					Balance:        property.Balance,
					PublicKey:      info.PubKey,
					DerivationPath: path,
				},
			)
		}
	}
	return addresses, nil
}

// CreateAccount creates a new account within the given key scope, deriving the
// account's keys from the wallet's master key.
//
// In contrast to ImportAccount, which registers a watch-only account from an
// externally supplied extended public key, the account created here is fully
// owned by the wallet: it derives its own addresses and can sign for its own
// outputs. That makes it usable as an isolated pocket of funds inside a single
// wallet, because coin selection, change, balance and address derivation can
// all be scoped to it by name.
//
// NOTE: The wallet must be unlocked, as deriving the account key requires
// access to the master private key.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) CreateAccount(keyScope waddrmgr.KeyScope,
	name string) (*waddrmgr.AccountProperties, error) {

	if name == "" {
		return nil, errors.New("account name is required")
	}

	// The wallet creates both of these accounts itself, in every key scope,
	// and neither is backed by a derived account key we could recreate
	// here.
	if name == lnwallet.DefaultAccountName ||
		name == waddrmgr.ImportedAddrAccountName {

		return nil, fmt.Errorf("account name %v is reserved by the "+
			"wallet", name)
	}

	// Everything below reads and then mutates the account namespace, and
	// btcwallet cannot do that in one database transaction, so hold the
	// lock across both. It only serialises callers within this process;
	// nothing stops a second process driving the same wallet, but lnd is
	// the sole writer of its own.
	b.accountMtx.Lock()
	defer b.accountMtx.Unlock()

	// Reject a duplicate name in *any* key scope, not just the requested
	// one. Coin selection resolves a custom account name through
	// lookupFirstCustomAccount, which returns whichever scope happens to
	// match first, so the same name existing under two scopes would make
	// every later funding call for that name ambiguous. btcwallet's own
	// duplicate check is per-scope, so it would not catch that.
	_, err := b.ListAccounts(name, nil)
	switch {
	case err == nil:
		return nil, fmt.Errorf("account %v already exists", name)

	// The name is free in every scope, which is what we want.
	case waddrmgr.IsError(err, waddrmgr.ErrAccountNotFound):

	default:
		return nil, err
	}

	// NewAccount already derives, persists and returns the account
	// snapshot; no legacy capability assertion or follow-up property
	// transaction remains.
	account, err := b.wallet.NewAccount(
		context.Background(), base.NewAccountParams{
			Scope: keyScope,
			Name:  name,
		},
	)
	if err != nil {
		return nil, fmt.Errorf(
			"unable to create account %v: %w",
			name,
			err,
		)
	}

	return accountProperties(*account)
}

// ImportAccount imports an account backed by an account extended public key.
// The master key fingerprint denotes the fingerprint of the root key
// corresponding to the account public key (also known as the key with
// derivation path m/). This may be required by some hardware wallets for proper
// identification and signing.
//
// The address type can usually be inferred from the key's version, but may be
// required for certain keys to map them into the proper scope.
//
// For custom accounts, we will first check if there is no account with the same
// name (even with a different key scope). No custom account should have various
// key scopes as it will result in non-deterministic behaviour.
//
// For BIP-0044 keys, an address type must be specified as we intend to not
// support importing BIP-0044 keys into the wallet using the legacy
// pay-to-pubkey-hash (P2PKH) scheme. A nested witness address type will force
// the standard BIP-0049 derivation scheme, while a witness address type will
// force the standard BIP-0084 derivation scheme.
//
// For BIP-0049 keys, an address type must also be specified to make a
// distinction between the standard BIP-0049 address schema (nested witness
// pubkeys everywhere) and our own BIP-0049Plus address schema (nested pubkeys
// externally, witness pubkeys internally).
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ImportAccount(name string, accountPubKey *hdkeychain.ExtendedKey,
	masterKeyFingerprint uint32, addrType *waddrmgr.AddressType,
	dryRun bool) (*waddrmgr.AccountProperties, []address.Address,
	[]address.Address, error) {

	// This shares the account namespace with CreateAccount and does the
	// same check-then-act against it, so it takes the same lock; see the
	// field's documentation.
	b.accountMtx.Lock()
	defer b.accountMtx.Unlock()

	// For custom accounts, we first check if there is no existing account
	// with the same name.
	if name != lnwallet.DefaultAccountName &&
		name != waddrmgr.ImportedAddrAccountName {

		_, err := b.ListAccounts(name, nil)
		if err == nil {
			return nil, nil, nil,
				fmt.Errorf("account '%s' already exists",
					name)
		}
		if !waddrmgr.IsError(err, waddrmgr.ErrAccountNotFound) {
			return nil, nil, nil, err
		}
	}

	// Let the maintained import validate custody and infer versions. Its
	// dry-run snapshot supplies the effective schema without persisting
	// keys.
	walletAddrType := waddrmgr.AddressType(0)
	if addrType != nil {
		walletAddrType = *addrType
	}
	info, err := b.wallet.ImportAccount(context.Background(), name,
		accountPubKey, masterKeyFingerprint, walletAddrType, dryRun)
	if err != nil {
		return nil, nil, nil, err
	}
	props, err := accountProperties(*info)
	if err != nil || !dryRun {
		return props, nil, nil, err
	}

	// Preview both branches directly from the supplied XPub. No wallet
	// allocation, watch installation or private key material is involved.
	var previews [2][]address.Address
	for branch := range previews {
		branchKey, err := accountPubKey.Derive(uint32(branch))
		if err != nil {
			return nil, nil, nil, err
		}
		addrType := info.AddrSchema.ExternalAddrType
		if branch == 1 {
			addrType = info.AddrSchema.InternalAddrType
		}
		for index := uint32(0); ; index++ {
			if len(previews[branch]) ==
				dryRunImportAccountNumAddrs {

				break
			}

			child, err := branchKey.Derive(index)
			if errors.Is(err, hdkeychain.ErrInvalidChild) {
				continue
			}
			if err != nil {
				return nil, nil, nil, err
			}
			pubKey, err := child.ECPubKey()
			if err != nil {
				return nil, nil, nil, err
			}
			addr, err := addrType.AddrFromPubKeyBytes(
				pubKey.SerializeCompressed(), b.netParams,
			)
			if err != nil {
				return nil, nil, nil, err
			}
			previews[branch] = append(previews[branch], addr)
		}
	}

	return props, previews[0], previews[1], nil
}

// ImportPublicKey imports a single derived public key into the wallet. The
// address type can usually be inferred from the key's version, but in the case
// of legacy versions (xpub, tpub), an address type must be specified as we
// intend to not support importing BIP-44 keys into the wallet using the legacy
// pay-to-pubkey-hash (P2PKH) scheme.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ImportPublicKey(pubKey *btcec.PublicKey,
	addrType waddrmgr.AddressType) error {

	return b.wallet.ImportPublicKey(context.Background(), pubKey, addrType)
}

// ImportTaprootScript imports a user-provided taproot script into the address
// manager. The imported script will act as a pay-to-taproot address.
func (b *BtcWallet) ImportTaprootScript(scope waddrmgr.KeyScope,
	tapscript *waddrmgr.Tapscript) (*base.AddressInfo, error) {

	// The maintained import is explicitly taproot; retain validation of the
	// existing lnd scope argument instead of silently accepting another
	// scope.
	if scope != waddrmgr.KeyScopeBIP0086 {
		return nil, fmt.Errorf("taproot scripts require " +
			"the BIP86 scope")
	}
	info, err := b.wallet.ImportTaprootScript(
		context.Background(),
		*tapscript,
	)
	if err != nil {
		return nil, err
	}

	return &info, nil
}

// SendOutputs funds, signs, and broadcasts a Bitcoin transaction paying out to
// the specified outputs. In the case the wallet has insufficient funds, or the
// outputs are non-standard, a non-nil error will be returned.
//
// NOTE: This method requires the global coin selection lock to be held.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) SendOutputs(inputs fn.Set[wire.OutPoint],
	outputs []*wire.TxOut, feeRate chainfee.SatPerKWeight,
	minConfs int32, label string,
	strategy base.CoinSelectionStrategy) (*wire.MsgTx, error) {

	// Reuse the unsigned authoring path and lnd's existing script signer.
	// Only a fully signed transaction reaches the maintained publisher.
	authored, err := b.CreateSimpleTx(
		inputs, outputs, feeRate, minConfs, strategy, false,
	)
	if err != nil {
		return nil, err
	}
	// RPCKeyRing supplies signatures for a watch-only wallet. Preserve its
	// unsigned-transaction handoff before attempting local signing.
	if b.cfg.Wallet.IsWatchOnly() {
		return authored.Tx, base.ErrTxUnsigned
	}
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for i, txIn := range authored.Tx.TxIn {
		fetcher.AddPrevOut(txIn.PreviousOutPoint, &wire.TxOut{
			Value:    int64(authored.PrevInputValues[i]),
			PkScript: authored.PrevScripts[i],
		})
	}
	sigHashes := txscript.NewTxSigHashes(authored.Tx, fetcher)
	for i, txIn := range authored.Tx.TxIn {
		output := fetcher.FetchPrevOutput(txIn.PreviousOutPoint)
		hashType := txscript.SigHashAll
		if txscript.IsPayToTaproot(output.PkScript) {
			hashType = txscript.SigHashDefault
		}
		signDesc := &input.SignDescriptor{
			Output:            output,
			InputIndex:        i,
			HashType:          hashType,
			SigHashes:         sigHashes,
			PrevOutputFetcher: fetcher,
		}
		script, err := b.ComputeInputScript(authored.Tx, signDesc)
		if err != nil {
			return nil, err
		}
		txIn.Witness = script.Witness
		txIn.SignatureScript = script.SigScript
	}
	if err := b.PublishTransaction(authored.Tx, label); err != nil {
		return nil, err
	}

	return authored.Tx, nil
}

// CreateSimpleTx creates a Bitcoin transaction paying to the specified
// outputs. The transaction is not broadcasted to the network, but a new change
// address might be created in the wallet database. In the case the wallet has
// insufficient funds, or the outputs are non-standard, an error should be
// returned. This method also takes the target fee expressed in sat/kw that
// should be used when crafting the transaction.
//
// NOTE: The dryRun argument can be set true to create a tx that doesn't alter
// the database. A tx created with this set to true SHOULD NOT be broadcasted.
//
// NOTE: This method requires the global coin selection lock to be held.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) CreateSimpleTx(inputs fn.Set[wire.OutPoint],
	outputs []*wire.TxOut, feeRate chainfee.SatPerKWeight, minConfs int32,
	strategy base.CoinSelectionStrategy, dryRun bool) (
	*txauthor.AuthoredTx, error) {

	// The fee rate is passed in using units of sat/kw, so we'll convert
	// this to sat/KB as the CreateSimpleTx method requires this unit.
	feeSatPerKB := btcutil.Amount(feeRate.FeePerKVByte())

	// Sanity check outputs.
	if len(outputs) < 1 {
		return nil, lnwallet.ErrNoOutputs
	}

	// Sanity check minConfs.
	if minConfs < 0 {
		return nil, lnwallet.ErrInvalidMinconf
	}

	for _, output := range outputs {
		// When checking an output for things like dusty-ness, we'll use
		// the default mempool relay fee rather than the target
		// effective fee rate to ensure accuracy. Otherwise, we may
		// mistakenly mark small-ish, but not quite dust output as dust.
		err := txrules.CheckOutput(
			output, txrules.DefaultRelayFeePerKb,
		)
		if err != nil {
			return nil, err
		}
	}

	if dryRun {
		return b.createSimpleTxDryRun(
			inputs, outputs, feeSatPerKB, minConfs, strategy,
		)
	}

	// Manual inputs are all required. Validate lnd's confirmation policy
	// before authoring because InputsManual intentionally has no min-confs.
	intent := &base.TxIntent{
		FeeRate: btcunit.NewSatPerKVByte(feeSatPerKB),
		Inputs: &base.InputsPolicy{
			Strategy: strategy,
			MinConfs: uint32(minConfs),
		},
	}
	for _, output := range outputs {
		intent.Outputs = append(intent.Outputs, *output)
	}
	if len(inputs) != 0 {
		for op := range inputs {
			utxo, err := b.wallet.GetUtxo(context.Background(), op)
			if err != nil {
				return nil, err
			}
			if utxo.Confirmations < minConfs {
				return nil, fmt.Errorf("selected input has " +
					"insufficient confirmations")
			}
		}
		intent.Inputs = &base.InputsManual{UTXOs: inputs.ToSlice()}
	} else {
		// lnd's default account spans its canonical address scopes. The
		// wallet's implicit source is BIP86 only, so supply the
		// eligible outpoints and let its existing policy choose among
		// them.
		utxos, err := b.ListUnspentWitness(
			minConfs, math.MaxInt32, lnwallet.DefaultAccountName,
		)
		if err != nil {
			return nil, err
		}
		var candidates []wire.OutPoint
		for _, utxo := range utxos {
			candidates = append(candidates, utxo.OutPoint)
		}
		intent.Inputs = &base.InputsPolicy{
			Strategy: strategy,
			MinConfs: uint32(minConfs),
			Source:   &base.CoinSourceUTXOs{UTXOs: candidates},
		}
	}

	return b.wallet.CreateTransaction(context.Background(), intent)
}

// createSimpleTxDryRun uses btcwallet's public authoring algorithm with
// read-only input and change sources. It previews the actual next internal key
// while leaving address cursors, output leases and transaction history
// untouched.
func (b *BtcWallet) createSimpleTxDryRun(selected fn.Set[wire.OutPoint],
	outputs []*wire.TxOut, fee btcutil.Amount, minConfs int32,
	strategy base.CoinSelectionStrategy) (*txauthor.AuthoredTx, error) {

	// Explicit inputs may belong to any account; automatic selection stays
	// within the default account, matching CreateSimpleTx's normal path.
	accountName := lnwallet.DefaultAccountName
	if len(selected) != 0 {
		accountName = ""
	}
	utxos, err := b.ListUnspentWitness(minConfs, math.MaxInt32, accountName)
	if err != nil {
		return nil, err
	}
	var coins []base.Coin
	for _, utxo := range utxos {
		unselected := len(selected) != 0 &&
			!selected.Contains(utxo.OutPoint)
		if unselected {
			continue
		}
		coins = append(coins, base.Coin{
			OutPoint: utxo.OutPoint,
			TxOut: wire.TxOut{
				Value:    int64(utxo.Value),
				PkScript: utxo.PkScript,
			},
		})
	}
	if len(selected) != 0 && len(coins) != len(selected) {
		return nil, fmt.Errorf("selected inputs are not all available")
	}
	if strategy == nil {
		strategy = base.CoinSelectionLargest
	}
	coins, err = strategy.ArrangeCoins(coins, fee)
	if err != nil {
		return nil, err
	}

	// The author may raise its target after estimating witness fees. Keep
	// accumulated inputs between calls, requiring every explicitly selected
	// coin even when an earlier prefix already covers the payment.
	var total btcutil.Amount
	var txInputs []*wire.TxIn
	var values []btcutil.Amount
	var scripts [][]byte
	inputSource := func(target btcutil.Amount) (btcutil.Amount,
		[]*wire.TxIn, []btcutil.Amount, [][]byte, error) {

		for len(coins) != 0 && (total < target || len(selected) != 0) {
			coin := coins[0]
			coins = coins[1:]
			txIn := wire.NewTxIn(&coin.OutPoint, nil, nil)
			txInputs = append(txInputs, txIn)
			total += btcutil.Amount(coin.Value)
			values = append(values, btcutil.Amount(coin.Value))
			scripts = append(scripts, coin.PkScript)
		}

		return total, txInputs, values, scripts, nil
	}

	// Actual default-account authoring chooses BIP86 change. Resolve the
	// persisted schema and next cursor so script size and fees match the
	// send.
	account, err := b.wallet.GetAccount(
		context.Background(), waddrmgr.KeyScopeBIP0086,
		lnwallet.DefaultAccountName,
	)
	if err != nil {
		return nil, err
	}
	pubKey, err := b.wallet.DerivePubKey(
		context.Background(), base.DerivePubKeyParams{
			Account: base.NewAccountSelectorByName(
				account.KeyScope, account.AccountName,
			),
			Branch: 1,
			Index:  account.InternalKeyCount,
		},
	)
	if err != nil {
		return nil, err
	}
	addr, err := account.AddrSchema.InternalAddrType.AddrFromPubKeyBytes(
		pubKey.SerializeCompressed(), b.netParams,
	)
	if err != nil {
		return nil, err
	}
	changeScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, err
	}

	return txauthor.NewUnsignedTransaction(
		outputs, fee, inputSource, &txauthor.ChangeSource{
			ScriptSize: len(changeScript),
			NewScript: func() ([]byte, error) {
				return changeScript, nil
			},
		},
	)
}

// LeaseOutput locks an output to the given ID, preventing it from being
// available for any future coin selection attempts. The absolute time of the
// lock's expiration is returned. The expiration of the lock can be extended by
// successive invocations of this call. Outputs can be unlocked before their
// expiration through `ReleaseOutput`.
//
// If the output is not known, wtxmgr.ErrUnknownOutput is returned. If the
// output has already been locked to a different ID, then
// wtxmgr.ErrOutputAlreadyLocked is returned.
//
// NOTE: This method requires the global coin selection lock to be held.
func (b *BtcWallet) LeaseOutput(id wtxmgr.LockID, op wire.OutPoint,
	duration time.Duration) (time.Time, error) {

	// The maintained lease operation checks ownership atomically; retain
	// the sentinels expected by existing lnd coin-selection callers.
	expiry, err := b.wallet.LeaseOutput(
		context.Background(),
		id,
		op,
		duration,
	)
	switch {
	case errors.Is(err, base.ErrUnknownOutput):
		return time.Time{}, wtxmgr.ErrUnknownOutput
	case errors.Is(err, base.ErrOutputAlreadyLocked):
		return time.Time{}, wtxmgr.ErrOutputAlreadyLocked
	default:
		return expiry, err
	}
}

// LeaseOutputWithOptions locks an output and applies optional persisted lease
// behavior supported by btcwallet. It returns wtxmgr.ErrUnknownOutput if the
// output is unknown and wtxmgr.ErrOutputAlreadyLocked if another owner holds
// its lease.
//
// NOTE: This method requires the global coin selection lock to be held.
func (b *BtcWallet) LeaseOutputWithOptions(id wtxmgr.LockID,
	op wire.OutPoint, duration time.Duration,
	opts lnwallet.LeaseOutputOptions) (time.Time, error) {

	// A timed lease cannot preserve release-after-spend semantics. Refuse
	// that option while continuing to serve ordinary duration-only leases.
	if opts.ReleaseAfterSpendConfs != 0 {
		return time.Time{}, fmt.Errorf("managed wallet " +
			"does not support " +
			"release-after-spend leases")
	}

	return b.LeaseOutput(id, op, duration)
}

// ListLeasedOutputs returns a list of all currently locked outputs.
func (b *BtcWallet) ListLeasedOutputs() ([]*base.ListLeasedOutputResult,
	error) {

	leases, err := b.wallet.ListLeasedOutputs(context.Background())
	if err != nil {
		return nil, err
	}
	// Resolve parent outputs for callers that total the locked balance.
	results := make([]*base.ListLeasedOutputResult, 0, len(leases))
	for _, lease := range leases {
		tx, err := b.FetchTx(lease.OutPoint.Hash)
		if err != nil {
			return nil, err
		}
		if uint64(lease.OutPoint.Index) >= uint64(len(tx.TxOut)) {
			return nil, fmt.Errorf("leased output is absent " +
				"from parent transaction")
		}
		output := tx.TxOut[lease.OutPoint.Index]
		results = append(results, &base.ListLeasedOutputResult{
			LockedOutput: &wtxmgr.LockedOutput{
				Outpoint:   lease.OutPoint,
				LockID:     lease.LockID,
				Expiration: lease.Expiration,
			},
			Value:    output.Value,
			PkScript: output.PkScript,
		})
	}

	return results, nil
}

// ReleaseOutput unlocks an output, allowing it to be available for coin
// selection if it remains unspent. The ID should match the one used to
// originally lock the output.
//
// NOTE: This method requires the global coin selection lock to be held.
func (b *BtcWallet) ReleaseOutput(id wtxmgr.LockID, op wire.OutPoint) error {
	err := b.wallet.ReleaseOutput(context.Background(), id, op)
	if errors.Is(err, base.ErrUnknownOutput) {
		return wtxmgr.ErrUnknownOutput
	}
	if errors.Is(err, base.ErrOutputUnlockNotAllowed) {
		return wtxmgr.ErrOutputUnlockNotAllowed
	}

	return err
}

// ListUnspentWitness returns all unspent outputs which are version 0 witness
// programs. The 'minConfs' and 'maxConfs' parameters indicate the minimum
// and maximum number of confirmations an output needs in order to be returned
// by this method. Passing -1 as 'minConfs' indicates that even unconfirmed
// outputs should be returned. Using MaxInt32 as 'maxConfs' implies returning
// all outputs with at least 'minConfs'. The account parameter serves as a
// filter to retrieve the unspent outputs for a specific account.  When empty,
// the unspent outputs of all wallet accounts are returned.
//
// NOTE: This method requires the global coin selection lock to be held.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListUnspentWitness(minConfs, maxConfs int32,
	accountFilter string) ([]*lnwallet.Utxo, error) {

	// First, grab all the unfiltered currently unspent outputs.
	unspentOutputs, err := b.wallet.ListUnspent(
		context.Background(), base.UtxoQuery{
			MinConfs: max(minConfs, 0),
			MaxConfs: maxConfs,
			Account:  accountFilter,
		},
	)
	if err != nil {
		return nil, err
	}

	// Next, we'll run through all the regular outputs, only saving those
	// which are p2wkh outputs or a p2wsh output nested within a p2sh output.
	witnessOutputs := make([]*lnwallet.Utxo, 0, len(unspentOutputs))
	for _, output := range unspentOutputs {
		// Local key availability does not determine spendability when
		// RPCKeyRing supplies signatures. Keep locks and local-wallet
		// exclusions while exposing remote-signable witness outputs.
		if output.Locked || (!output.Spendable && !b.cfg.WatchOnly) {
			continue
		}
		// Spendable also encodes coinbase maturity. The maintained UTXO
		// view omits that origin. Inspect the parent when remote
		// signing bypasses this flag and maturity is still in question.
		if !output.Spendable && output.Confirmations <
			int32(b.netParams.CoinbaseMaturity) {

			parent, err := b.FetchTx(output.OutPoint.Hash)
			if err != nil {
				return nil, err
			}
			if blockchain.IsCoinBaseTx(parent) {
				continue
			}
		}
		pkScript := output.PkScript

		addressType := lnwallet.UnknownAddressType
		if txscript.IsPayToWitnessPubKeyHash(pkScript) {
			addressType = lnwallet.WitnessPubKey
		} else if txscript.IsPayToScriptHash(pkScript) {
			// TODO(roasbeef): This assumes all p2sh outputs returned by the
			// wallet are nested p2pkh. We can't check the redeem script because
			// the btcwallet service does not include it.
			addressType = lnwallet.NestedWitnessPubKey
		} else if txscript.IsPayToTaproot(pkScript) {
			addressType = lnwallet.TaprootPubkey
		}

		if addressType == lnwallet.WitnessPubKey ||
			addressType == lnwallet.NestedWitnessPubKey ||
			addressType == lnwallet.TaprootPubkey {

			utxo := &lnwallet.Utxo{
				AddressType:   addressType,
				Value:         output.Amount,
				PkScript:      pkScript,
				OutPoint:      output.OutPoint,
				Confirmations: int64(output.Confirmations),
			}
			witnessOutputs = append(witnessOutputs, utxo)
		}

	}

	return witnessOutputs, nil
}

// mapRpcclientError maps an error from the `btcwallet/chain` package to
// defined error in this package.
//
// NOTE: we are mapping the errors returned from `sendrawtransaction` RPC or
// the reject reason from `testmempoolaccept` RPC.
func mapRpcclientError(err error) error {
	// If we failed to publish the transaction, check whether we got an
	// error of known type.
	switch {
	// If the wallet reports a double spend, convert it to our internal
	// ErrDoubleSpend and return.
	case errors.Is(err, chain.ErrMempoolConflict),
		errors.Is(err, chain.ErrMissingInputs),
		errors.Is(err, chain.ErrTxAlreadyKnown),
		errors.Is(err, chain.ErrTxAlreadyConfirmed):

		return lnwallet.ErrDoubleSpend

	// If the wallet reports that fee requirements for accepting the tx
	// into mempool are not met, convert it to our internal ErrMempoolFee
	// and return.
	case errors.Is(err, chain.ErrMempoolMinFeeNotMet),
		errors.Is(err, chain.ErrMinRelayFeeNotMet):

		return fmt.Errorf("%w: %v", lnwallet.ErrMempoolFee, err.Error())
	}

	return err
}

// PublishTransaction performs cursory validation (dust checks, etc), then
// finally broadcasts the passed transaction to the Bitcoin network. If
// publishing the transaction fails, an error describing the reason is returned
// and mapped to the wallet's internal error types. If the transaction is
// already published to the network (either in the mempool or chain) no error
// will be returned.
func (b *BtcWallet) PublishTransaction(tx *wire.MsgTx, label string) error {
	// For neutrino backend there's no mempool, so we return early by
	// publishing the transaction.
	if b.chain.BackEnd() == "neutrino" {
		err := b.wallet.Broadcast(context.Background(), tx, label)

		return mapRpcclientError(err)
	}

	// For non-neutrino nodes, we will first check whether the transaction
	// can be accepted by the mempool.
	// Use a max feerate of 0 means the default value will be used when
	// testing mempool acceptance. The default max feerate is 0.10 BTC/kvb,
	// or 10,000 sat/vb.
	results, err := b.chain.TestMempoolAccept([]*wire.MsgTx{tx}, 0)
	if err != nil {
		// If the chain backend doesn't support the mempool acceptance
		// test RPC, we'll just attempt to publish the transaction.
		if errors.Is(err, rpcclient.ErrBackendVersion) {
			log.Warnf("TestMempoolAccept not supported by "+
				"backend, consider upgrading %s to a newer "+
				"version", b.chain.BackEnd())

			err := b.wallet.Broadcast(
				context.Background(),
				tx,
				label,
			)

			return mapRpcclientError(err)
		}

		return err
	}

	// Sanity check that the expected single result is returned.
	if len(results) != 1 {
		return fmt.Errorf("expected 1 result from TestMempoolAccept, "+
			"instead got %v", len(results))
	}

	result := results[0]
	log.Debugf("TestMempoolAccept result: %s",
		lnutils.SpewLogClosure(result))

	// Once mempool check passed, we can publish the transaction.
	if result.Allowed {
		err = b.wallet.Broadcast(context.Background(), tx, label)

		return mapRpcclientError(err)
	}

	// If the check failed, there's no need to publish it. We'll handle the
	// error and return.
	log.Warnf("Transaction %v not accepted by mempool: %v",
		tx.TxHash(), result.RejectReason)

	// We need to use the string to create an error type and map it to a
	// btcwallet error.
	err = b.chain.MapRPCErr(errors.New(result.RejectReason))

	//nolint:ll
	// These two errors are ignored inside `PublishTransaction`:
	// https://github.com/btcsuite/btcwallet/blob/master/wallet/wallet.go#L3763
	// To keep our current behavior, we need to ignore the same errors
	// returned from TestMempoolAccept.
	//
	// TODO(yy): since `LightningWallet.PublishTransaction` always publish
	// the same tx twice, we'd always get ErrTxAlreadyInMempool. We should
	// instead create a new rebroadcaster that monitors the mempool, and
	// only rebroadcast when the tx is evicted. This way we don't need to
	// broadcast twice, and can instead return these errors here.
	switch {
	// NOTE: In addition to ignoring these errors, we need to call
	// `PublishTransaction` again because we need to mark the label in the
	// wallet. We can remove this exception once we have the above TODO
	// fixed.
	case errors.Is(err, chain.ErrTxAlreadyInMempool),
		errors.Is(err, chain.ErrTxAlreadyKnown),
		errors.Is(err, chain.ErrTxAlreadyConfirmed):

		err := b.wallet.Broadcast(context.Background(), tx, label)
		return mapRpcclientError(err)
	}

	return mapRpcclientError(err)
}

// neutrinoBroadcastMsg is the PackageMsg returned by the neutrino best-effort
// path. It is deliberately not "success": a neutrino light client has no
// mempool, so it cannot confirm the package was accepted. It broadcasts the
// transactions and reports them as broadcast-but-unverified, which callers
// must treat as an unverified relay attempt, not a package-accept verdict.
const neutrinoBroadcastMsg = "broadcast-unverified"

// SubmitPackage submits a package of related transactions (topologically
// sorted, parents first and child last) for atomic validation and acceptance.
//
// Only the bitcoind backend performs real package submission, via the node's
// submitpackage RPC, which lets a zero-fee v3/TRUC parent be accepted via its
// fee-paying CPFP child (which sendrawtransaction rejects on its own). The
// btcd backend has no submitpackage handler and returns ErrUnimplemented.
//
// A neutrino light client has no mempool and cannot validate or atomically
// accept a package. As a best effort it broadcasts each transaction
// individually over the P2P network and relies on a peer's 1p1c package relay
// to assemble them. The returned PackageMsg is deliberately not "success": a
// light client cannot confirm acceptance, so callers must treat the result as
// an unverified broadcast rather than a package-accept verdict.
func (b *BtcWallet) SubmitPackage(txns []*wire.MsgTx,
	maxFeeRate *chainfee.SatPerVByte) (*btcjson.SubmitPackageResult,
	error) {

	if b.chain.BackEnd() == "neutrino" {
		// The best-effort neutrino broadcast goes through plain
		// SendRawTransaction, which cannot enforce a fee-rate ceiling,
		// so reject a caller-provided limit rather than silently
		// ignoring it and giving a false sense of protection.
		if maxFeeRate != nil {
			return nil, fmt.Errorf("max fee rate is not " +
				"supported for neutrino package broadcast")
		}

		for i, tx := range txns {
			if err := b.PublishTransaction(tx, ""); err != nil {
				return nil, fmt.Errorf("unable to "+
					"broadcast package tx %d (%v): %w",
					i, tx.TxHash(), err)
			}
		}

		results := make(
			map[string]btcjson.SubmitPackageTxResult, len(txns),
		)
		for _, tx := range txns {
			results[tx.WitnessHash().String()] =
				btcjson.SubmitPackageTxResult{TxID: tx.TxHash()}
		}

		return &btcjson.SubmitPackageResult{
			PackageMsg: neutrinoBroadcastMsg,
			TxResults:  results,
		}, nil
	}

	// bitcoind's submitpackage maxfeerate is expressed in BTC/kvB, so map
	// the optional sat/vByte ceiling onto it. A nil ceiling leaves the node
	// default unchanged; an explicit 0 disables the limit.
	var maxFeeRateBTCPerKvB *float64
	if maxFeeRate != nil {
		btcPerKvB := satPerVByteToBTCPerKvB(*maxFeeRate)
		maxFeeRateBTCPerKvB = &btcPerKvB
	}

	return b.chain.SubmitPackage(txns, maxFeeRateBTCPerKvB)
}

// vBytesPerKvB is the number of virtual bytes in a kilo-virtual-byte, used to
// convert a sat/vByte fee rate into the per-kvB unit bitcoind expects.
const vBytesPerKvB = 1000

// satPerVByteToBTCPerKvB converts a sat/vByte fee rate into the BTC/kvB unit
// expected by bitcoind's submitpackage maxfeerate argument: 1 sat/vByte is
// 1000 sat/kvB, and SatoshiPerBitcoin sats make a BTC, so
// BTC/kvB = sat/vByte * 1000 / SatoshiPerBitcoin.
//
// NOTE: the sat/vByte input is integer, so only whole-sat/vByte ceilings are
// expressible, and very large values lose precision once the float64 product
// exceeds 2^53.
func satPerVByteToBTCPerKvB(rate chainfee.SatPerVByte) float64 {
	return float64(rate) * vBytesPerKvB / btcutil.SatoshiPerBitcoin
}

// LabelTransaction adds a label to a transaction. If the tx already
// has a label, this call will fail unless the overwrite parameter
// is set. Labels must not be empty, and they are limited to 500 chars.
//
// Note: it is part of the WalletController interface.
func (b *BtcWallet) LabelTransaction(hash chainhash.Hash, label string,
	overwrite bool) error {

	// Read the current label before the existing overwrite=false operation.
	// Empty labels remain invalid at the lnd boundary even though LabelTx
	// supports clearing labels for other wallet consumers.
	if label == "" {
		return fmt.Errorf("transaction label must not be empty")
	}
	if !overwrite {
		tx, err := b.wallet.GetTx(context.Background(), hash)
		if errors.Is(err, base.ErrTxNotFound) {
			return base.ErrUnknownTransaction
		}
		if err != nil {
			return err
		}
		if tx.Label != "" {
			return base.ErrTxLabelExists
		}
	}

	return b.wallet.LabelTx(context.Background(), hash, label)
}

// GetTransactionDetails returns details of a transaction given its
// transaction hash.
func (b *BtcWallet) GetTransactionDetails(
	txHash *chainhash.Hash) (*lnwallet.TransactionDetail, error) {

	tx, err := b.wallet.GetTx(context.Background(), *txHash)
	if err != nil {
		return nil, err
	}

	return transactionDetail(tx), nil
}

// transactionDetail translates the maintained history snapshot without
// recomputing ownership or fees from legacy notification summaries.
func transactionDetail(tx *base.TxDetail) *lnwallet.TransactionDetail {
	detail := &lnwallet.TransactionDetail{
		Hash:             tx.Hash,
		Value:            tx.Value,
		NumConfirmations: tx.Confirmations,
		Timestamp:        tx.ReceivedTime.Unix(),
		TotalFees:        int64(tx.Fee),
		RawTx:            tx.RawTx,
		Label:            tx.Label,
	}
	if tx.Block != nil {
		detail.BlockHash = &tx.Block.Hash
		detail.BlockHeight = tx.Block.Height
		detail.Timestamp = tx.Block.Timestamp
	}
	for _, output := range tx.Outputs {
		detail.OutputDetails = append(
			detail.OutputDetails, lnwallet.OutputDetail{
				OutputType:   output.Type,
				Addresses:    output.Addresses,
				PkScript:     output.PkScript,
				OutputIndex:  output.Index,
				Value:        output.Amount,
				IsOurAddress: output.IsOurs,
			},
		)
	}
	for _, prev := range tx.PrevOuts {
		detail.PreviousOutpoints = append(
			detail.PreviousOutpoints, lnwallet.PreviousOutPoint{
				OutPoint:    prev.OutPoint.String(),
				IsOurOutput: prev.IsOurs,
			},
		)
	}

	return detail
}

// transactionDetailsPage applies the requested offset and limit to a set of
// transaction details. A zero limit means that all remaining transactions are
// returned.
func transactionDetailsPage(txDetails []*lnwallet.TransactionDetail,
	indexOffset, maxTransactions uint32) ([]*lnwallet.TransactionDetail,
	uint64, uint64) {

	total := uint64(len(txDetails))
	first := uint64(indexOffset)
	if first >= total {
		return []*lnwallet.TransactionDetail{}, 0, 0
	}

	end := total
	if maxTransactions != 0 {
		// Compare the limit to the remaining count. This avoids adding
		// caller-controlled values before deciding whether to clamp the
		// requested end.
		limit := uint64(maxTransactions)
		remaining := total - first
		if limit < remaining {
			end = first + limit
		}
	}

	// Both bounds are no greater than len(txDetails), so these conversions
	// are safe on both 32-bit and 64-bit platforms.
	page := txDetails[int(first):int(end)]

	return page, first, end - 1
}

// ListTransactionDetails returns a list of all transactions which are relevant
// to the wallet over [startHeight;endHeight]. If start height is greater than
// end height, the transactions will be retrieved in reverse order. To include
// unconfirmed transactions, endHeight should be set to the special value -1.
// This will return transactions from the tip of the chain until the start
// height (inclusive) and unconfirmed transactions. The account parameter serves
// as a filter to retrieve the transactions relevant to a specific account. When
// empty, transactions of all wallet accounts are returned.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) ListTransactionDetails(startHeight, endHeight int32,
	accountFilter string, indexOffset uint32,
	maxTransactions uint32) ([]*lnwallet.TransactionDetail, uint64, uint64,
	error) {

	// Account filtering uses the existing address inventory. Input
	// ownership remains available after spending through the parent's
	// stored transaction, so filtering does not depend on an output
	// remaining in the UTXO set.
	accountAddresses := fn.NewSet[string]()
	if accountFilter != "" {
		accounts, err := b.ListAddresses(accountFilter, false)
		if err != nil {
			return nil, 0, 0, err
		}
		for _, addresses := range accounts {
			for _, addr := range addresses {
				accountAddresses.Add(addr.Address)
			}
		}
	}
	matchesScript := func(script []byte) bool {
		_, addresses, _, err := txscript.ExtractPkScriptAddrs(
			script,
			b.netParams,
		)
		if err != nil {
			return false
		}

		return slices.ContainsFunc(
			addresses, func(addr address.Address) bool {
				return accountAddresses.Contains(addr.String())
			},
		)
	}
	txns, err := b.wallet.ListTxns(
		context.Background(),
		startHeight,
		endHeight,
	)
	if err != nil {
		return nil, 0, 0, err
	}
	txDetails := make([]*lnwallet.TransactionDetail, 0, len(txns))
	for _, tx := range txns {
		matches := accountFilter == ""
		for _, output := range tx.Outputs {
			matches = matches || matchesScript(output.PkScript)
		}
		if !matches {
			for _, prev := range tx.PrevOuts {
				if !prev.IsOurs {
					continue
				}
				parent, err := b.FetchTx(prev.OutPoint.Hash)
				if err != nil {
					return nil, 0, 0, err
				}
				index := uint64(prev.OutPoint.Index)
				if index >= uint64(len(parent.TxOut)) {
					err := fmt.Errorf("wallet input " +
						"is absent from parent " +
						"transaction")

					return nil, 0, 0, err
				}
				script := parent.TxOut[index].PkScript
				matches = matchesScript(script)
				if matches {
					break
				}
			}
		}
		if matches {
			txDetails = append(txDetails, transactionDetail(tx))
		}
	}

	page, firstIndex, lastIndex := transactionDetailsPage(
		txDetails, indexOffset, maxTransactions,
	)

	return page, firstIndex, lastIndex, nil
}

// SubscribeTransactions returns a TransactionSubscription client which
// is capable of receiving async notifications as new transactions
// related to the wallet are seen within the network, or found in
// blocks.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) SubscribeTransactions() (lnwallet.TransactionSubscription, error) {
	events, err := b.wallet.SubscribeTxns()
	if err != nil {
		return nil, err
	}
	sub := &managedTxSubscription{
		events:      events,
		confirmed:   make(chan *lnwallet.TransactionDetail, 64),
		unconfirmed: make(chan *lnwallet.TransactionDetail, 64),
		quit:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	go sub.run()
	return sub, nil
}

// managedTxSubscription maps the diagnostic btcwallet stream to lnd's wallet
// contract while allowing cancellation to release the forwarding goroutine.
type managedTxSubscription struct {
	events      *base.TxSubscription
	confirmed   chan *lnwallet.TransactionDetail
	unconfirmed chan *lnwallet.TransactionDetail
	quit        chan struct{}
	done        chan struct{}
	once        sync.Once
}

// ConfirmedTransactions returns confirmed wallet transaction notifications.
func (s *managedTxSubscription) ConfirmedTransactions() chan *lnwallet.TransactionDetail {
	return s.confirmed
}

// UnconfirmedTransactions returns unconfirmed wallet transaction notifications.
func (s *managedTxSubscription) UnconfirmedTransactions() chan *lnwallet.TransactionDetail {
	return s.unconfirmed
}

// Cancel stops forwarding and unregisters the wallet subscription.
func (s *managedTxSubscription) Cancel() {
	s.once.Do(func() {
		close(s.quit)
		s.events.Done()
		<-s.done
	})
}

// run forwards committed wallet details until cancellation or wallet shutdown.
func (s *managedTxSubscription) run() {
	defer close(s.done)
	for {
		select {
		case <-s.quit:
			return
		case tx, ok := <-s.events.C:
			if !ok {
				return
			}
			detail := transactionDetail(tx)
			out := s.unconfirmed
			if tx.Block != nil {
				out = s.confirmed
			}
			select {
			case out <- detail:
			case <-s.quit:
				return
			}
		}
	}
}

// IsSynced returns a boolean indicating if from the PoV of the wallet, it has
// fully synced to the current best block in the main chain.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) IsSynced() (bool, int64, error) {
	// Info binds sync readiness to the source's observed tip. Keep lnd's
	// timestamp freshness check in addition to that managed sync status.
	info, err := b.wallet.Info(context.Background())
	if err != nil {
		return false, 0, err
	}
	bestTimestamp := info.SyncedTo.Timestamp.Unix()
	if !info.Synced {
		return false, bestTimestamp, nil
	}
	blockHeader, err := b.chain.GetBlockHeader(&info.SyncedTo.Hash)
	if err != nil {
		return false, 0, err
	}

	// If the timestamp on the best header is more than 2 hours in the
	// past, then we're not yet synced.
	minus24Hours := time.Now().Add(-2 * time.Hour)
	if blockHeader.Timestamp.Before(minus24Hours) {
		return false, bestTimestamp, nil
	}

	return true, bestTimestamp, nil
}

// GetRecoveryInfo returns a boolean indicating whether the wallet is started
// in recovery mode. It also returns a float64, ranging from 0 to 1,
// representing the recovery progress made so far.
//
// This is a part of the WalletController interface.
func (b *BtcWallet) GetRecoveryInfo() (bool, float64, error) {
	isRecoveryMode := true
	progress := float64(0)

	// A zero value in RecoveryWindow indicates there is no trigger of
	// recovery mode.
	if b.cfg.RecoveryWindow == 0 {
		isRecoveryMode = false
		return isRecoveryMode, progress, nil
	}

	// Use the maintained controller snapshot for both the birthday and
	// scanned tip, retaining the existing progress calculation below.
	info, err := b.wallet.Info(context.Background())
	if err != nil {
		return isRecoveryMode, progress, err
	}
	birthdayBlock := info.BirthdayBlock
	syncState := info.SyncedTo

	// Next, query the chain backend to grab the info about the tip of the
	// main chain.
	//
	// NOTE: The actual recovery process is handled by the btcsuite/btcwallet.
	// The process purposefully doesn't update the best height. It might create
	// a small difference between the height queried here and the height used
	// in the recovery process, ie, the bestHeight used here might be greater,
	// showing the recovery being unfinished while it's actually done. However,
	// during a wallet rescan after the recovery, the wallet's synced height
	// will catch up and this won't be an issue.
	_, bestHeight, err := b.cfg.ChainSource.GetBestBlock()
	if err != nil {
		return isRecoveryMode, progress, err
	}

	// The birthday block height might be greater than the current synced height
	// in a newly restored wallet, and might be greater than the chain tip if a
	// rollback happens. In that case, we will return zero progress here.
	if syncState.Height < birthdayBlock.Height ||
		bestHeight < birthdayBlock.Height {

		return isRecoveryMode, progress, nil
	}

	// progress is the ratio of the [number of blocks processed] over the [total
	// number of blocks] needed in a recovery mode, ranging from 0 to 1, in
	// which,
	// - total number of blocks is the current chain's best height minus the
	//   wallet's birthday height plus 1.
	// - number of blocks processed is the wallet's synced height minus its
	//   birthday height plus 1.
	// - If the wallet is born very recently, the bestHeight can be equal to
	//   the birthdayBlock.Height, and it will recovery instantly.
	progress = float64(syncState.Height-birthdayBlock.Height+1) /
		float64(bestHeight-birthdayBlock.Height+1)

	return isRecoveryMode, progress, nil
}

// FetchTx attempts to fetch a transaction in the wallet's database identified
// by the passed transaction hash. If the transaction can't be found, then a
// nil pointer is returned.
func (b *BtcWallet) FetchTx(txHash chainhash.Hash) (*wire.MsgTx, error) {
	tx, err := b.wallet.GetTx(context.Background(), txHash)
	if err != nil {
		return nil, err
	}

	wireTx := wire.NewMsgTx(2)
	if err := wireTx.Deserialize(bytes.NewReader(tx.RawTx)); err != nil {
		return nil, err
	}

	return wireTx, nil
}

// RemoveDescendants attempts to remove any transaction from the wallet's tx
// store (that may be unconfirmed) that spends outputs created by the passed
// transaction. This remove propagates recursively down the chain of descendent
// transactions.
func (b *BtcWallet) RemoveDescendants(tx *wire.MsgTx) error {
	// DeleteUnconfirmedTx also deletes its argument. Find only direct
	// children first so the caller's root transaction remains in history.
	history, err := b.wallet.ListTxns(context.Background(), 0, -1)
	if err != nil {
		return err
	}
	root := tx.TxHash()
	for _, child := range history {
		if child.Confirmations != 0 {
			continue
		}
		for _, prev := range child.PrevOuts {
			if prev.OutPoint.Hash != root {
				continue
			}
			err := b.wallet.DeleteUnconfirmedTx(
				context.Background(), child.Hash,
			)
			if err != nil && !errors.Is(err, base.ErrTxNotFound) {
				return err
			}

			break
		}
	}

	return nil
}

// CheckMempoolAcceptance is a wrapper around `TestMempoolAccept` which checks
// the mempool acceptance of a transaction.
func (b *BtcWallet) CheckMempoolAcceptance(tx *wire.MsgTx) error {
	// Use a max feerate of 0 means the default value will be used when
	// testing mempool acceptance. The default max feerate is 0.10 BTC/kvb,
	// or 10,000 sat/vb.
	results, err := b.chain.TestMempoolAccept([]*wire.MsgTx{tx}, 0)
	if err != nil {
		return err
	}

	// Sanity check that the expected single result is returned.
	if len(results) != 1 {
		return fmt.Errorf("expected 1 result from TestMempoolAccept, "+
			"instead got %v", len(results))
	}

	result := results[0]
	log.Debugf("TestMempoolAccept result: %s",
		lnutils.SpewLogClosure(result))

	// Mempool check failed, we now map the reject reason to a proper RPC
	// error and return it.
	if !result.Allowed {
		err := b.chain.MapRPCErr(errors.New(result.RejectReason))

		return fmt.Errorf("mempool rejection: %w", err)
	}

	return nil
}
