package walletunlocker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/macaroons"
)

var (
	// ErrUnlockTimeout signals that we did not get the expected unlock
	// message before the timeout occurred.
	ErrUnlockTimeout = errors.New("got no unlock message before timeout")
)

// WalletUnlockParams holds the variables used to parameterize the unlocking of
// lnd's wallet after it has already been created.
type WalletUnlockParams struct {
	// Password is the public and private wallet passphrase.
	Password []byte

	// Birthday specifies the approximate time that this wallet was created.
	// This is used to bound any rescans on startup.
	Birthday time.Time

	// RecoveryWindow specifies the address lookahead when entering recovery
	// mode. A recovery will be attempted if this value is non-zero.
	RecoveryWindow uint32

	// Wallet is the loaded and unlocked Wallet. This is returned
	// from the unlocker service to avoid it being unlocked twice (once in
	// the unlocker service to check if the password is correct and again
	// later when lnd actually uses it). Because unlocking involves scrypt
	// which is resource intensive, we want to avoid doing it twice.
	Wallet *wallet.Wallet

	// Manager owns the handed-off wallet runtime and database.
	Manager *wallet.Manager

	// ChansToRestore a set of static channel backups that should be
	// restored before the main server instance starts up.
	ChansToRestore ChannelsToRecover

	// UnloadWallet is a function for unloading the wallet, which should
	// be called on shutdown.
	UnloadWallet func() error

	// StatelessInit signals that the user requested the daemon to be
	// initialized stateless, which means no unencrypted macaroons should be
	// written to disk.
	StatelessInit bool

	// MacResponseChan is the channel for sending back the admin macaroon to
	// the WalletUnlocker service.
	MacResponseChan chan []byte

	// MacRootKey is the 32 byte macaroon root key specified by the user
	// during wallet initialization.
	MacRootKey []byte
}

// ChannelsToRecover wraps any set of packed (serialized+encrypted) channel
// back ups together. These can be passed in when unlocking the wallet, or
// creating a new wallet for the first time with an existing seed.
type ChannelsToRecover struct {
	// PackedMultiChanBackup is an encrypted and serialized multi-channel
	// backup.
	PackedMultiChanBackup chanbackup.PackedMulti

	// PackedSingleChanBackups is a series of encrypted and serialized
	// single-channel backup for one or more channels.
	PackedSingleChanBackups chanbackup.PackedSingles
}

// WalletInitMsg is a message sent by the UnlockerService when a user wishes to
// set up the internal wallet for the first time. The user MUST provide a
// passphrase, but is also able to provide their own source of entropy. If
// provided, then this source of entropy will be used to generate the wallet's
// HD seed. Otherwise, the wallet will generate one itself.
type WalletInitMsg struct {
	// Passphrase is the passphrase that will be used to encrypt the wallet
	// itself. This MUST be at least 8 characters.
	Passphrase []byte

	// WalletSeed is the deciphered cipher seed that the wallet should use
	// to initialize itself. The seed might be nil if the wallet should be
	// created from an extended master root key instead.
	WalletSeed *aezeed.CipherSeed

	// WalletExtendedKey is the wallet's extended master root key that
	// should be used instead of the seed, if non-nil. The extended key is
	// mutually exclusive to the wallet seed, but one of both is always set.
	WalletExtendedKey *hdkeychain.ExtendedKey

	// ExtendedKeyBirthday is the birthday of a wallet that's being restored
	// through an extended key instead of an aezeed.
	ExtendedKeyBirthday time.Time

	// WatchOnlyAccounts is a map of scoped account extended public keys
	// that should be imported to create a watch-only wallet.
	WatchOnlyAccounts map[waddrmgr.ScopedIndex]*hdkeychain.ExtendedKey

	// WatchOnlyBirthday is the birthday of the master root key the above
	// watch-only account xpubs were derived from.
	WatchOnlyBirthday time.Time

	// WatchOnlyMasterFingerprint is the fingerprint of the master root key
	// the above watch-only account xpubs were derived from.
	WatchOnlyMasterFingerprint uint32

	// RecoveryWindow is the address look-ahead used when restoring a seed
	// with existing funds. A recovery window zero indicates that no
	// recovery should be attempted, such as after the wallet's initial
	// creation.
	RecoveryWindow uint32

	// ChanBackups a set of static channel backups that should be received
	// after the wallet has been initialized.
	ChanBackups ChannelsToRecover

	// StatelessInit signals that the user requested the daemon to be
	// initialized stateless, which means no unencrypted macaroons should be
	// written to disk.
	StatelessInit bool

	// MacRootKey is the 32 byte macaroon root key specified by the user
	// during wallet initialization.
	MacRootKey []byte
}

// WalletUnlockMsg is a message sent by the UnlockerService when a user wishes
// to unlock the internal wallet after initial setup. The user can optionally
// specify a recovery window, which will resume an interrupted rescan for used
// addresses.
type WalletUnlockMsg struct {
	// Passphrase is the passphrase that will be used to encrypt the wallet
	// itself. This MUST be at least 8 characters.
	Passphrase []byte

	// RecoveryWindow is the address look-ahead used when restoring a seed
	// with existing funds. A recovery window zero indicates that no
	// recovery should be attempted, such as after the wallet's initial
	// creation, but before any addresses have been created.
	RecoveryWindow uint32

	// Wallet is the loaded and unlocked Wallet. This is returned through
	// the channel to avoid it being unlocked twice (once to check if the
	// password is correct, here in the WalletUnlocker and again later when
	// lnd actually uses it). Because unlocking involves scrypt which is
	// resource intensive, we want to avoid doing it twice.
	Wallet *wallet.Wallet

	// Manager owns the handed-off wallet runtime and database.
	Manager *wallet.Manager

	// ChanBackups a set of static channel backups that should be received
	// after the wallet has been unlocked.
	ChanBackups ChannelsToRecover

	// UnloadWallet is a function for unloading the wallet, which should
	// be called on shutdown.
	UnloadWallet func() error

	// StatelessInit signals that the user requested the daemon to be
	// initialized stateless, which means no unencrypted macaroons should be
	// written to disk.
	StatelessInit bool
}

// UnlockerService implements the WalletUnlocker service used to provide lnd
// with a password for wallet encryption at startup. Additionally, during
// initial setup, users can provide their own source of entropy which will be
// used to generate the seed that's ultimately used within the wallet.
type UnlockerService struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	lnrpc.UnimplementedWalletUnlockerServer

	// InitMsgs is a channel that carries all wallet init messages.
	InitMsgs chan *WalletInitMsg

	// UnlockMsgs is a channel where unlock parameters provided by the rpc
	// client to be used to unlock and decrypt an existing wallet will be
	// sent.
	UnlockMsgs chan *WalletUnlockMsg

	// MacResponseChan is the channel for sending back the admin macaroon to
	// the WalletUnlocker service.
	MacResponseChan chan []byte

	netParams *chaincfg.Params

	// macaroonFiles is the path to the three generated macaroons with
	// different access permissions. These might not exist in a stateless
	// initialization of lnd.
	macaroonFiles []string

	// resetWalletTransactions indicates that the wallet state should be
	// reset on unlock to force a full chain rescan.
	resetWalletTransactions bool

	// managerConfig selects storage and the immutable startup policy.
	managerConfig *wallet.ManagerConfig

	// manager and currentWallet borrow native SQL startup's owned runtime
	// and its published Create/Start result. Password retries reuse both.
	manager       *wallet.Manager
	currentWallet *atomic.Pointer[wallet.Wallet]

	// macaroonDB is an instance of a database backend that stores all
	// macaroon root keys. This will be nil on initialization and must be
	// set using the SetMacaroonDB method as soon as it's available.
	macaroonDB kvdb.Backend
}

// New creates and returns a new UnlockerService.
func New(params *chaincfg.Params, macaroonFiles []string,
	resetWalletTransactions bool,
	managerConfig *wallet.ManagerConfig) *UnlockerService {

	return &UnlockerService{
		InitMsgs:   make(chan *WalletInitMsg, 1),
		UnlockMsgs: make(chan *WalletUnlockMsg, 1),

		// Make sure we buffer the channel is buffered so the main lnd
		// goroutine isn't blocking on writing to it.
		MacResponseChan:         make(chan []byte, 1),
		netParams:               params,
		macaroonFiles:           macaroonFiles,
		resetWalletTransactions: resetWalletTransactions,
		managerConfig:           managerConfig,
	}
}

// SetManagerConfig supplies wallet startup policy and, for native SQL, borrows
// the one started Manager and its wallet result before serving unlock requests.
func (u *UnlockerService) SetManagerConfig(cfg wallet.ManagerConfig,
	manager *wallet.Manager, current *atomic.Pointer[wallet.Wallet]) {

	u.managerConfig = &cfg
	u.manager = manager
	u.currentWallet = current
}

// SetMacaroonDB can be used to inject the macaroon database after the unlocker
// service has been hooked to the main RPC server.
func (u *UnlockerService) SetMacaroonDB(macaroonDB kvdb.Backend) {
	u.macaroonDB = macaroonDB
}

// newManager reuses native SQL startup or starts the local walletdb Manager
// after the request supplies its public passphrase and recovery policy.
func (u *UnlockerService) newManager(publicPass []byte,
	recoveryWindow uint32) (*wallet.Manager, *wallet.Wallet, error) {

	if u.managerConfig == nil {
		return nil, nil, fmt.Errorf("wallet manager is not configured")
	}
	if u.managerConfig.Backend != wallet.DBBackendKVDB {
		if recoveryWindow != 0 {
			return nil, nil, fmt.Errorf("native wallet " +
				"historical " +
				"recovery requires accounts to exist before " +
				"synchronization")
		}
		if u.manager == nil || u.currentWallet == nil {
			return nil, nil, fmt.Errorf("native wallet manager " +
				"is not started")
		}

		return u.manager, u.currentWallet.Load(), nil
	}

	cfg := *u.managerConfig
	cfg.KVDBPubPassphrase = publicPass
	cfg.RecoveryWindow = recoveryWindow
	manager, err := wallet.NewManager(context.Background(), cfg)
	if err != nil {
		return nil, nil, err
	}
	wallets, err := manager.Start(context.Background())
	if err != nil || len(wallets) != 1 {
		_ = manager.Stop()
		if err != nil {
			return nil, nil, err
		}

		return nil, nil, fmt.Errorf("expected one initialized wallet")
	}

	return manager, wallets[0], nil
}

// WalletExists uses the native Manager's durable wallet set. A local walletdb
// still uses its file boundary because its public passphrase is not known yet.
func (u *UnlockerService) WalletExists() (bool, error) {
	if u.managerConfig == nil {
		return false, fmt.Errorf("wallet manager is not configured")
	}
	if u.managerConfig.Backend != wallet.DBBackendKVDB {
		if u.currentWallet == nil {
			return false, fmt.Errorf("native wallet manager " +
				"is not started")
		}

		return u.currentWallet.Load() != nil, nil
	}
	_, err := os.Stat(u.managerConfig.DataSource)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

// GenSeed is the first method that should be used to instantiate a new lnd
// instance. This method allows a caller to generate a new aezeed cipher seed
// given an optional passphrase. If provided, the passphrase will be necessary
// to decrypt the cipherseed to expose the internal wallet seed.
//
// Once the cipherseed is obtained and verified by the user, the InitWallet
// method should be used to commit the newly generated seed, and create the
// wallet.
func (u *UnlockerService) GenSeed(_ context.Context,
	in *lnrpc.GenSeedRequest) (*lnrpc.GenSeedResponse, error) {

	// Before we start, we'll ensure that the wallet hasn't already created
	// so we don't show a *new* seed to the user if one already exists.
	walletExists, err := u.WalletExists()
	if err != nil {
		return nil, err
	}
	if walletExists {
		return nil, fmt.Errorf("wallet already exists")
	}

	var entropy [aezeed.EntropySize]byte

	switch {
	// If the user provided any entropy, then we'll make sure it's sized
	// properly.
	case len(in.SeedEntropy) != 0 && len(in.SeedEntropy) != aezeed.EntropySize:
		return nil, fmt.Errorf("incorrect entropy length: expected "+
			"16 bytes, instead got %v bytes", len(in.SeedEntropy))

	// If the user provided the correct number of bytes, then we'll copy it
	// over into our buffer for usage.
	case len(in.SeedEntropy) == aezeed.EntropySize:
		copy(entropy[:], in.SeedEntropy[:])

	// Otherwise, we'll generate a fresh new set of bytes to use as entropy
	// to generate the seed.
	default:
		if _, err := rand.Read(entropy[:]); err != nil {
			return nil, err
		}
	}

	// Now that we have our set of entropy, we'll create a new cipher seed
	// instance.
	//
	cipherSeed, err := aezeed.New(
		keychain.CurrentKeyDerivationVersion, &entropy, time.Now(),
	)
	if err != nil {
		return nil, err
	}

	// With our raw cipher seed obtained, we'll convert it into an encoded
	// mnemonic using the user specified pass phrase.
	mnemonic, err := cipherSeed.ToMnemonic(in.AezeedPassphrase)
	if err != nil {
		return nil, err
	}

	// Additionally, we'll also obtain the raw enciphered cipher seed as
	// well to return to the user.
	encipheredSeed, err := cipherSeed.Encipher(in.AezeedPassphrase)
	if err != nil {
		return nil, err
	}

	return &lnrpc.GenSeedResponse{
		CipherSeedMnemonic: mnemonic[:],
		EncipheredSeed:     encipheredSeed[:],
	}, nil
}

// extractChanBackups is a helper function that extracts the set of channel
// backups from the proto into a format that we'll pass to higher level
// sub-systems.
func extractChanBackups(chanBackups *lnrpc.ChanBackupSnapshot) *ChannelsToRecover {
	// If there aren't any populated channel backups, then we can exit
	// early as there's nothing to extract.
	if chanBackups == nil || (chanBackups.SingleChanBackups == nil &&
		chanBackups.MultiChanBackup == nil) {
		return nil
	}

	// Now that we know there's at least a single back up populated, we'll
	// extract the multi-chan backup (if it's there).
	var backups ChannelsToRecover
	if chanBackups.MultiChanBackup != nil {
		multiBackup := chanBackups.MultiChanBackup
		backups.PackedMultiChanBackup = multiBackup.MultiChanBackup
	}

	if chanBackups.SingleChanBackups == nil {
		return &backups
	}

	// Finally, we can extract all the single chan backups as well.
	for _, backup := range chanBackups.SingleChanBackups.ChanBackups {
		singleChanBackup := backup.ChanBackup

		backups.PackedSingleChanBackups = append(
			backups.PackedSingleChanBackups, singleChanBackup,
		)
	}

	return &backups
}

// InitWallet is used when lnd is starting up for the first time to fully
// initialize the daemon and its internal wallet. At the very least a wallet
// password must be provided. This will be used to encrypt sensitive material
// on disk.
//
// In the case of a recovery scenario, the user can also specify their aezeed
// mnemonic and passphrase. If set, then the daemon will use this prior state
// to initialize its internal wallet.
//
// Alternatively, this can be used along with the GenSeed RPC to obtain a
// seed, then present it to the user. Once it has been verified by the user,
// the seed can be fed into this RPC in order to commit the new wallet.
func (u *UnlockerService) InitWallet(ctx context.Context,
	in *lnrpc.InitWalletRequest) (*lnrpc.InitWalletResponse, error) {

	// Make sure the password meets our constraints.
	password := in.WalletPassword
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}

	// Require that the recovery window be non-negative.
	recoveryWindow := in.RecoveryWindow
	if recoveryWindow < 0 {
		return nil, fmt.Errorf("recovery window %d must be "+
			"non-negative", recoveryWindow)
	}

	// Ensure that the macaroon root key is *exactly* 32-bytes.
	macaroonRootKey := in.MacaroonRootKey
	if len(macaroonRootKey) > 0 &&
		len(macaroonRootKey) != macaroons.RootKeyLen {

		return nil, fmt.Errorf("macaroon root key must be exactly "+
			"%v bytes, is instead %v",
			macaroons.RootKeyLen, len(macaroonRootKey),
		)
	}

	// Native SQL begins synchronization before this RPC, so historical
	// recovery cannot promise discovery of accounts created afterward.
	if u.managerConfig != nil &&
		u.managerConfig.Backend != wallet.DBBackendKVDB &&
		recoveryWindow != 0 {

		return nil, fmt.Errorf("native wallet historical recovery " +
			"requires accounts to exist before synchronization")
	}
	walletExists, err := u.WalletExists()
	if err != nil {
		return nil, err
	}

	// If the wallet already exists, then we'll exit early as we can't
	// create the wallet if it already exists!
	if walletExists {
		return nil, fmt.Errorf("wallet already exists")
	}

	// At this point, we know the wallet doesn't already exist so we can
	// prepare the message that we'll send over the channel later.
	initMsg := &WalletInitMsg{
		Passphrase:     password,
		RecoveryWindow: uint32(recoveryWindow),
		StatelessInit:  in.StatelessInit,
		MacRootKey:     macaroonRootKey,
	}

	// There are two supported ways to initialize the wallet. Either from
	// the aezeed or the final extended master key directly.
	switch {
	// Don't allow the user to specify both as that would be ambiguous.
	case len(in.CipherSeedMnemonic) > 0 && len(in.ExtendedMasterKey) > 0:
		return nil, fmt.Errorf("cannot specify both the cipher " +
			"seed mnemonic and the extended master key")

	// The aezeed is the preferred and default way of initializing a wallet.
	case len(in.CipherSeedMnemonic) > 0:
		// We'll map the user provided aezeed and passphrase into a
		// decoded cipher seed instance.
		var mnemonic aezeed.Mnemonic
		copy(mnemonic[:], in.CipherSeedMnemonic)

		// If we're unable to map it back into the ciphertext, then
		// either the mnemonic is wrong, or the passphrase is wrong.
		cipherSeed, err := mnemonic.ToCipherSeed(in.AezeedPassphrase)
		if err != nil {
			return nil, err
		}

		initMsg.WalletSeed = cipherSeed

	// To support restoring a wallet where the seed isn't known or a wallet
	// created externally to lnd, we also allow the extended master key
	// (xprv) to be imported directly. This is what'll be stored in the
	// btcwallet database anyway.
	case len(in.ExtendedMasterKey) > 0:
		extendedKey, err := hdkeychain.NewKeyFromString(
			in.ExtendedMasterKey,
		)
		if err != nil {
			return nil, err
		}

		// The on-chain wallet of lnd is going to derive keys based on
		// the BIP49/84 key derivation paths from this root key. To make
		// sure we use default derivation paths, we want to avoid
		// deriving keys from something other than the master key (at
		// depth 0, denoted with "m/" in BIP32 notation).
		if extendedKey.Depth() != 0 {
			return nil, fmt.Errorf("extended master key must " +
				"be at depth 0 not a child key")
		}

		// Because we need the master key (at depth 0), it must be an
		// extended private key as the first levels of BIP49/84
		// derivation paths are hardened, which isn't possible with
		// extended public keys.
		if !extendedKey.IsPrivate() {
			return nil, fmt.Errorf("extended master key must " +
				"contain private keys")
		}

		// To avoid using the wrong master key, we check that it was
		// issued for the correct network. This will cause problems if
		// someone tries to import a "new" BIP84 zprv key because with
		// this we only support the "legacy" zprv prefix. But it is
		// trivial to convert between those formats, as long as the user
		// knows what they're doing.
		if !extendedKey.IsForNet(u.netParams) {
			return nil, fmt.Errorf("extended master key must be "+
				"for network %s", u.netParams.Name)
		}

		// When importing a wallet from its extended private key we
		// don't know the birthday as that information is not encoded in
		// that format. We therefore must set an arbitrary date to start
		// rescanning at if the user doesn't provide an explicit value
		// for it. Since lnd only uses SegWit addresses, we pick the
		// date of the first block that contained SegWit transactions
		// (481824).
		initMsg.ExtendedKeyBirthday = time.Date(
			2017, time.August, 24, 1, 57, 37, 0, time.UTC,
		)
		if in.ExtendedMasterKeyBirthdayTimestamp != 0 {
			initMsg.ExtendedKeyBirthday = time.Unix(
				int64(in.ExtendedMasterKeyBirthdayTimestamp), 0,
			)
		}

		initMsg.WalletExtendedKey = extendedKey

	// The third option for creating a wallet is the watch-only mode:
	// Instead of providing the master root key directly, each individual
	// account is passed as an extended public key only. Because of the
	// hardened derivation path up to the account (depth 3), it is not
	// possible to create a master root extended _public_ key. Therefore, an
	// xpub must be derived and passed into the unlocker for _every_ account
	// lnd expects.
	case in.WatchOnly != nil && len(in.WatchOnly.Accounts) > 0:
		initMsg.WatchOnlyAccounts = make(
			map[waddrmgr.ScopedIndex]*hdkeychain.ExtendedKey,
			len(in.WatchOnly.Accounts),
		)

		for _, acct := range in.WatchOnly.Accounts {
			scopedIndex := waddrmgr.ScopedIndex{
				Scope: waddrmgr.KeyScope{
					Purpose: acct.Purpose,
					Coin:    acct.CoinType,
				},
				Index: acct.Account,
			}
			acctKey, err := hdkeychain.NewKeyFromString(acct.Xpub)
			if err != nil {
				return nil, fmt.Errorf("error parsing xpub "+
					"%v: %v", acct.Xpub, err)
			}

			// Just to make sure the user is doing the right thing,
			// we expect the public key to be at derivation depth
			// three (which is the account level) and the key not to
			// contain any private key material.
			if acctKey.Depth() != 3 {
				return nil, fmt.Errorf("xpub must be at " +
					"depth 3")
			}
			if acctKey.IsPrivate() {
				return nil, fmt.Errorf("xpub is not really " +
					"an xpub, contains private key")
			}

			initMsg.WatchOnlyAccounts[scopedIndex] = acctKey
		}

		// When importing a wallet from its extended public keys we
		// don't know the birthday as that information is not encoded in
		// that format. We therefore must set an arbitrary date to start
		// rescanning at if the user doesn't provide an explicit value
		// for it. Since lnd only uses SegWit addresses, we pick the
		// date of the first block that contained SegWit transactions
		// (481824).
		initMsg.WatchOnlyBirthday = time.Date(
			2017, time.August, 24, 1, 57, 37, 0, time.UTC,
		)
		if in.WatchOnly.MasterKeyBirthdayTimestamp != 0 {
			initMsg.WatchOnlyBirthday = time.Unix(
				int64(in.WatchOnly.MasterKeyBirthdayTimestamp),
				0,
			)
		}

	// No key material was set, no wallet can be created.
	default:
		return nil, fmt.Errorf("must either specify cipher seed " +
			"mnemonic or the extended master key")
	}

	// Before we return the unlock payload, we'll check if we can extract
	// any channel backups to pass up to the higher level sub-system.
	chansToRestore := extractChanBackups(in.ChannelBackups)
	if chansToRestore != nil {
		initMsg.ChanBackups = *chansToRestore
	}

	// Deliver the initialization message back to the main daemon.
	select {
	case u.InitMsgs <- initMsg:
		// We need to read from the channel to let the daemon continue
		// its work and to get the admin macaroon. Once the response
		// arrives, we directly forward it to the client.
		select {
		case adminMac := <-u.MacResponseChan:
			return &lnrpc.InitWalletResponse{
				AdminMacaroon: adminMac,
			}, nil

		case <-ctx.Done():
			return nil, ErrUnlockTimeout
		}

	case <-ctx.Done():
		return nil, ErrUnlockTimeout
	}
}

// LoadAndUnlock authenticates the private passphrase on the started wallet. Its
// cleanup locks a borrowed native wallet or closes the request-owned kvdb
// Manager; successful handoff transfers that Manager to normal node shutdown.
func (u *UnlockerService) LoadAndUnlock(password []byte,
	recoveryWindow uint32) (*wallet.Wallet, *wallet.Manager, func() error,
	error) {

	if u.resetWalletTransactions {
		return nil, nil, nil, fmt.Errorf("managed wallet " +
			"does not support " +
			"transaction-history reset")
	}
	exists, err := u.WalletExists()
	if err != nil {
		return nil, nil, nil, err
	}
	if !exists {
		return nil, nil, nil, fmt.Errorf("wallet not found")
	}
	manager, w, err := u.newManager(password, recoveryWindow)
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := manager.Stop
	if u.managerConfig.Backend != wallet.DBBackendKVDB {
		cleanup = func() error {
			return w.Lock(context.Background())
		}
	}
	// An already-unlocked error must remain an authentication failure.
	// Never clear another request's private state when this unlock fails.
	err = w.Unlock(context.Background(), wallet.UnlockRequest{
		Passphrase: password,
		Timeout:    -1,
	})
	if err != nil {
		if u.managerConfig.Backend == wallet.DBBackendKVDB {
			_ = manager.Stop()
		}

		return nil, nil, nil, err
	}

	return w, manager, cleanup, nil
}

// UnlockWallet sends the password provided by the incoming UnlockWalletRequest
// over the UnlockMsgs channel in case it successfully decrypts an existing
// wallet found in the chain's wallet database directory.
func (u *UnlockerService) UnlockWallet(ctx context.Context,
	in *lnrpc.UnlockWalletRequest) (*lnrpc.UnlockWalletResponse, error) {

	password := in.WalletPassword
	recoveryWindow := uint32(in.RecoveryWindow)

	unlockedWallet, manager, unloadFn, err := u.LoadAndUnlock(
		password, recoveryWindow,
	)
	if err != nil {
		return nil, err
	}

	// Cancellation before handoff releases this attempt's private state;
	// after handoff the node owns the runtime independently of the RPC.
	if ctx.Err() != nil {
		_ = unloadFn()
		return nil, ErrUnlockTimeout
	}

	// We successfully opened the wallet and pass the instance back to
	// avoid it needing to be unlocked again.
	walletUnlockMsg := &WalletUnlockMsg{
		Passphrase:     password,
		RecoveryWindow: recoveryWindow,
		Wallet:         unlockedWallet,
		Manager:        manager,
		UnloadWallet:   unloadFn,
		StatelessInit:  in.StatelessInit,
	}

	// Before we return the unlock payload, we'll check if we can extract
	// any channel backups to pass up to the higher level sub-system.
	chansToRestore := extractChanBackups(in.ChannelBackups)
	if chansToRestore != nil {
		walletUnlockMsg.ChanBackups = *chansToRestore
	}

	// At this point we were able to open the existing wallet with the
	// provided password. We send the password over the UnlockMsgs
	// channel, such that it can be used by lnd to open the wallet.
	select {
	case u.UnlockMsgs <- walletUnlockMsg:
		// We need to read from the channel to let the daemon continue
		// its work. But we don't need the returned macaroon for this
		// operation, so we read it but then discard it.
		select {
		case <-u.MacResponseChan:
			return &lnrpc.UnlockWalletResponse{}, nil

		case <-ctx.Done():
			return nil, ErrUnlockTimeout
		}

	case <-ctx.Done():
		_ = unloadFn()
		return nil, ErrUnlockTimeout
	}
}

// ChangePassword changes the password of the wallet and sends the new password
// across the UnlockPasswords channel to automatically unlock the wallet if
// successful.
func (u *UnlockerService) ChangePassword(ctx context.Context,
	in *lnrpc.ChangePasswordRequest) (*lnrpc.ChangePasswordResponse, error) {

	// Reject cancellation before rotating either credential store. Once
	// rotation begins, wallet and macaroon updates must finish together.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	walletExists, err := u.WalletExists()
	if err != nil {
		return nil, err
	}

	if !walletExists {
		return nil, errors.New("wallet not found")
	}

	publicPw := in.CurrentPassword
	privatePw := in.CurrentPassword

	// If the current password is blank, we'll assume the user is coming
	// from a --noseedbackup state, so we'll use the default passwords.
	if len(in.CurrentPassword) == 0 {
		publicPw = lnwallet.DefaultPublicPassphrase
		privatePw = lnwallet.DefaultPrivatePassphrase
	}

	// Make sure the new password meets our constraints.
	if err := ValidatePassword(in.NewPassword); err != nil {
		return nil, err
	}

	// Load the existing wallet in order to proceed with the password change.
	manager, w, err := u.newManager(publicPw, 0)
	if err != nil {
		return nil, err
	}

	// Now that we've opened the wallet, we need to close it in case of an
	// error. But not if we succeed, then the caller must close it.
	orderlyReturn := false
	defer func() {
		if !orderlyReturn {
			if u.managerConfig.Backend == wallet.DBBackendKVDB {
				_ = manager.Stop()
			} else {
				_ = w.Lock(context.Background())
			}
		}
	}()

	// Native storage opens before private authentication. Validate the old
	// secret before deleting macaroon files, then restore the locked state
	// expected by the final unlock after both credential stores rotate.
	if u.managerConfig.Backend != wallet.DBBackendKVDB {
		err = w.Unlock(context.WithoutCancel(ctx), wallet.UnlockRequest{
			Passphrase: privatePw,
			Timeout:    -1,
		})
		if err != nil {
			// Failed admission acquired no private state to clean
			// up. In particular, do not relock another admitted
			// request.
			orderlyReturn = true
			return nil, err
		}
		if err := w.Lock(context.WithoutCancel(ctx)); err != nil {
			return nil, err
		}
	}

	// Before we actually change the password, we need to check if all flags
	// were set correctly. The content of the previously generated macaroon
	// files will become invalid after we generate a new root key. So we try
	// to delete them here and they will be recreated during normal startup
	// later. If they are missing, this is only an error if the
	// stateless_init flag was not set.
	if in.NewMacaroonRootKey || in.StatelessInit {
		for _, file := range u.macaroonFiles {
			err := os.Remove(file)
			if err != nil && !in.StatelessInit {
				return nil, fmt.Errorf("could not remove "+
					"macaroon file: %v. if the wallet "+
					"was initialized stateless please "+
					"add the --stateless_init "+
					"flag", err)
			}
		}
	}

	// Attempt to change both the public and private passphrases for the
	// wallet. This will be done atomically in order to prevent one
	// passphrase change from being successful and not the other.
	changePublic := u.managerConfig.Backend == wallet.DBBackendKVDB
	err = w.ChangePassphrase(context.WithoutCancel(ctx),
		wallet.ChangePassphraseRequest{
			ChangePublic:  changePublic,
			PublicOld:     publicPw,
			PublicNew:     in.NewPassword,
			ChangePrivate: true,
			PrivateOld:    privatePw,
			PrivateNew:    in.NewPassword,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("unable to change wallet passphrase: "+
			"%w", err)
	}

	// The next step is to load the macaroon database, change the password
	// then close it again.
	// Attempt to open the macaroon DB, unlock it and then change
	// the passphrase.
	rootKeyStore, err := macaroons.NewRootKeyStorage(u.macaroonDB)
	if err != nil {
		return nil, err
	}
	macaroonService, err := macaroons.NewService(
		rootKeyStore, "lnd", in.StatelessInit,
	)
	if err != nil {
		return nil, err
	}

	err = macaroonService.CreateUnlock(&privatePw)
	if err != nil {
		closeErr := macaroonService.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("could not create unlock: %v "+
				"--> follow-up error when closing: %v", err,
				closeErr)
		}
		return nil, err
	}
	err = macaroonService.ChangePassword(privatePw, in.NewPassword)
	if err != nil {
		closeErr := macaroonService.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("could not change password: %v "+
				"--> follow-up error when closing: %v", err,
				closeErr)
		}
		return nil, err
	}

	// If requested by the user, attempt to replace the existing
	// macaroon root key with a new one.
	if in.NewMacaroonRootKey {
		err = macaroonService.GenerateNewRootKey()
		if err != nil {
			closeErr := macaroonService.Close()
			if closeErr != nil {
				return nil, fmt.Errorf("could not generate "+
					"new root key: %v --> follow-up error "+
					"when closing: %v", err, closeErr)
			}
			return nil, err
		}
	}

	err = macaroonService.Close()
	if err != nil {
		return nil, fmt.Errorf("could not close macaroon service: %w",
			err)
	}

	// Authenticate the new credential for the same unlocked-wallet handoff
	// used by UnlockWallet, only after macaroon rotation has completed.
	err = w.Unlock(context.WithoutCancel(ctx), wallet.UnlockRequest{
		Passphrase: in.NewPassword,
		Timeout:    -1,
	})
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ErrUnlockTimeout
	}

	// Finally, send the new password across the UnlockPasswords channel to
	// automatically unlock the wallet.
	walletUnlockMsg := &WalletUnlockMsg{
		Passphrase:    in.NewPassword,
		Wallet:        w,
		StatelessInit: in.StatelessInit,
		UnloadWallet:  manager.Stop,
		Manager:       manager,
	}
	select {
	case u.UnlockMsgs <- walletUnlockMsg:
		// We need to read from the channel to let the daemon continue
		// its work and to get the admin macaroon. Once the response
		// arrives, we directly forward it to the client.
		orderlyReturn = true
		select {
		case adminMac := <-u.MacResponseChan:
			return &lnrpc.ChangePasswordResponse{
				AdminMacaroon: adminMac,
			}, nil

		case <-ctx.Done():
			return nil, ErrUnlockTimeout
		}

	case <-ctx.Done():
		return nil, ErrUnlockTimeout
	}
}

// ValidatePassword assures the password meets all of our constraints.
func ValidatePassword(password []byte) error {
	// Passwords should have a length of at least 8 characters.
	if len(password) < 8 {
		return errors.New("password must have at least 8 characters")
	}

	return nil
}
