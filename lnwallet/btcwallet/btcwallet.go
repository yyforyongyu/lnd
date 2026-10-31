package btcwallet

import (
	"context"
	"fmt"
	"slices"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	base "github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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
)

// InitialAccounts declares payment and Lightning accounts before a native
// Wallet starts scanning. Preparing the full Lightning family set makes signer
// exports complete without account writes contending with the initial scan.
func InitialAccounts(coinType uint32) []base.NewAccountParams {
	accounts := make([]base.NewAccountParams, 0, len(LndDefaultKeyScopes))
	// Payment scopes keep their existing account zero and address schemas
	// so seed recovery scans the same on-chain derivation paths.
	for _, scope := range LndDefaultKeyScopes {
		zero := base.AccountNumber(0)
		schema := waddrmgr.ScopeAddrMap[scope]
		accounts = append(accounts, base.NewAccountParams{
			Scope:         scope,
			Name:          lnwallet.DefaultAccountName,
			AccountNumber: &zero,
			AddrSchema:    &schema,
		})
	}

	// Remote signers must export every hardened family because watch-only
	// imports cannot derive missing account keys. These off-chain accounts
	// use the existing names and never participate in payment-chain scans.
	for family := base.AccountNumber(0); family <= 255; family++ {
		name := fmt.Sprintf("act:%d", family)
		if family == 0 {
			name = lnwallet.DefaultAccountName
		}
		accounts = append(accounts, base.NewAccountParams{
			Scope: waddrmgr.KeyScope{
				Purpose: keychain.BIP0043Purpose,
				Coin:    coinType,
			},
			Name:          name,
			AccountNumber: &family,
			AddrSchema:    &lightningAddrSchema,
			NoChainSync:   true,
		})
	}

	return accounts
}

// walletAPI composes the maintained wallet capabilities used by this adapter.
// Embedding the existing interfaces keeps mock expectations at the same
// boundary without a second runtime or a forwarding implementation.
type walletAPI interface {
	lnwallet.WalletAPI
	base.UnsafeSigner
}

var _ walletAPI = (*base.Wallet)(nil)

// BtcWallet integrates Manager lifecycle, chain access and Lightning signing.
// Ordinary wallet callers borrow InternalWallet and its maintained interfaces.
type BtcWallet struct {
	// wallet is an active instance of btcwallet.
	wallet walletAPI

	chain chain.Interface

	cfg *Config

	netParams *chaincfg.Params

	chainKeyScope waddrmgr.KeyScope

	blockCache *blockcache.BlockCache

	*input.MusigSessionManager
}

// A compile time check to ensure that BtcWallet implements the
// application services and BlockChainIO interfaces.
var _ lnwallet.WalletServices = (*BtcWallet)(nil)
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
		// Prepare native accounts before recovery starts, so exporting
		// signer families does not compete with the initial chain scan.
		if cfg.ManagerConfig.Backend != base.DBBackendKVDB {
			params.InitialDerivedAccounts =
				InitialAccounts(cfg.CoinType)
			params.RecoveryWindow = &cfg.RecoveryWindow
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

// Start authenticates the Manager-owned Wallet and prepares payment and
// Lightning accounts. Manager has already started synchronization.
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
	// SQL watch-only wallets still encrypt scripts with a passphrase key.
	// Only the legacy kvdb watch-only vault has no unlockable material.
	legacyWatchOnly := walletIsWatchOnly &&
		b.cfg.ManagerConfig.Backend == base.DBBackendKVDB
	if info.Locked && !legacyWatchOnly {
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

	return nil
}

// Stop joins the Manager's wallet work and closes its database before stopping
// the borrowed chain source, which remains usable throughout wallet shutdown.
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
			err := b.wallet.Broadcast(context.Background(), tx, "")
			if err != nil {
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

// GetRecoveryInfo returns a boolean indicating whether the wallet is started
// in recovery mode. It also returns a float64, ranging from 0 to 1,
// representing the recovery progress made so far.
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
	// NOTE: The actual recovery process is handled by
	// btcsuite/btcwallet. The process purposefully doesn't update the
	// best height. It might create a small difference between the height
	// queried here and the height used in recovery, ie, bestHeight might
	// be greater, showing recovery as unfinished while it's actually done.
	// During a wallet rescan after recovery, the wallet's synced height
	// will catch up.
	_, bestHeight, err := b.cfg.ChainSource.GetBestBlock()
	if err != nil {
		return isRecoveryMode, progress, err
	}

	// The birthday block height might be greater than the current synced
	// height in a newly restored wallet, and might be greater than the
	// chain tip if a rollback happens. In that case, return zero progress.
	if syncState.Height < birthdayBlock.Height ||
		bestHeight < birthdayBlock.Height {

		return isRecoveryMode, progress, nil
	}

	// progress is the ratio of the [number of blocks processed] over the
	// [total number of blocks] needed in recovery, ranging from 0 to 1:
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
