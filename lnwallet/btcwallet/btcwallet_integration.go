//go:build integration || lowscrypt

package btcwallet

import (
	"github.com/btcsuite/btcwallet/snacl"
	"github.com/btcsuite/btcwallet/waddrmgr"
)

// init applies the explicitly selected cheap KDF mode before any Wallet starts.
// Ordinary production builds exclude this file and retain their default cost.
func init() {
	// Instruct waddrmgr to use the cranked down scrypt parameters when
	// creating new wallet encryption keys. This will speed up the itests
	// considerably.
	fastScrypt := waddrmgr.FastScryptOptions

	// Native SQL creates its secrets directly from these options rather
	// than the legacy hook below. Apply the same test cost to both paths
	// so SQL wallet creation respects the existing cheap-KDF build mode.
	waddrmgr.DefaultScryptOptions = fastScrypt

	keyGen := func(passphrase *[]byte, config *waddrmgr.ScryptOptions) (
		*snacl.SecretKey, error) {

		return snacl.NewSecretKey(
			passphrase, fastScrypt.N, fastScrypt.R, fastScrypt.P,
		)
	}
	waddrmgr.SetSecretKeyGen(keyGen)
}
