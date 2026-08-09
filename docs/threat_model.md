# LND Operator Credential Threat Model

## Purpose

This document models compromise of the secrets, credentials, processes, and
operator infrastructure surrounding an `lnd` node. Its purpose is to make
security claims explicit before new permissions, caveats, configuration
allowlists, or signing policies are designed.

The central question for every case is:

> Given exactly this attacker capability, what can `lnd` still prevent, what
> can it only mitigate, and what is already beyond recovery?

This is a design document, not a claim that every proposed mitigation is
implemented. Each case distinguishes current behavior from a desired security
property. New security features should cite the cases they address and state
which cases remain out of scope.

The broader operational guidance in [safety.md](safety.md),
[wallet.md](wallet.md), [macaroons.md](macaroons.md),
[recovery.md](recovery.md), and [remote-signing.md](remote-signing.md) remains
applicable.

## Contents

* [Scope](#scope)
* [Terminology](#terminology)
* [Assets and security objectives](#assets-and-security-objectives)
* [Trust boundaries and attacker classes](#trust-boundaries-and-attacker-classes)
* [Quick conclusions](#quick-conclusions)
* [Recovery and encryption secrets](#case-analysis-recovery-and-encryption-secrets)
* [Wallet runtime state](#case-analysis-wallet-runtime-state)
* [Macaroon exposure](#case-analysis-macaroon-exposure)
* [Transport and deployment](#case-analysis-transport-and-deployment)
* [Remote signing](#case-analysis-remote-signing)
* [Channel state, peers, and recovery](#case-analysis-channel-state-peers-and-recovery)
* [Security-sensitive RPC classes](#security-sensitive-rpc-classes)
* [Cross-case attack paths](#cross-case-attack-paths)
* [Control-to-threat mapping](#control-to-threat-mapping)
* [Design requirements](#design-requirements-derived-from-the-cases)
* [Adding and reviewing cases](#adding-and-reviewing-cases)
* [Open policy questions](#open-policy-questions)
* [Implementation references](#implementation-references)

## Scope

This document covers:

* The aezeed mnemonic and cipher seed passphrase.
* The wallet password and wallet lock state.
* Wallet, macaroon, channel, and backup files.
* Macaroon permissions, caveats, root keys, and revocation.
* RPC and TLS exposure.
* Application, service account, root, and hosting-provider compromise.
* Watch-only and remote-signer deployments.
* Security-sensitive RPC request fields and permission composition.

This document does not attempt to model every Lightning protocol or Bitcoin
consensus attack. Malicious peers, chain backends, and watchtowers are included
only where they cross an operator or credential trust boundary.

Implementation bugs and cryptographic breaks are also outside the base model.
They should be considered separately because they can invalidate any claimed
boundary.

## Terminology

### Secrets are not interchangeable

LND uses several independent secrets:

* The **aezeed mnemonic** encodes the encrypted root seed.
* The optional **cipher seed passphrase** decrypts the aezeed mnemonic. If no
  passphrase is supplied, the known default value `aezeed` is used.
* The **wallet password** encrypts private wallet data and the macaroon root
  keys stored on disk. It is independent from the cipher seed passphrase.
* A **macaroon root key** can mint bearer macaroons for its root key ID.
* A **macaroon** is a bearer credential. Possession is sufficient to exercise
  all authority encoded in it while its caveats are satisfied.
* The **TLS private key** authenticates the RPC server to clients. It does not
  authorize an RPC client.
* The **channel database** contains live, changing channel state that is not
  reconstructed from the seed.
* A **Static Channel Backup (SCB)** contains static recovery information and is
  encrypted to a key derived from the node seed.

Confusing these secrets leads to incorrect claims. For example, a cipher seed
passphrase protects a stolen mnemonic, but it does not lock a running wallet or
attenuate a stolen macaroon.

### Impact classes

The case analysis uses the following outcomes:

* **Observe**: learn balances, invoices, peers, transactions, addresses,
  channel data, or other private information.
* **Disrupt**: stop the node, reject traffic, lock liquidity, close channels,
  corrupt workflow state, or destroy accounting data.
* **Burn**: make the operator pay avoidable on-chain or routing fees without
  transferring the principal to the attacker.
* **Redirect**: transfer on-chain or off-chain value to an attacker-selected
  address, invoice, peer, output script, or key.
* **Authorize**: mint, replace, or revoke credentials and thereby change the
  attacker's future authority.
* **Recover**: determine whether the operator can recover funds and live
  channel state after an incident.

"End game" should always identify an impact. Root seed disclosure is end game
for deterministic key secrecy, but it does not reconstruct the latest channel
state. An admin macaroon is end game for the current RPC authorization model,
but it is not the aezeed mnemonic.

## Assets and security objectives

The assets, in descending order of severity, are:

1. On-chain principal and local channel balances.
2. Private keys, seed material, and unrestricted signing authority.
3. Current channel state and the ability to recover it safely.
4. Macaroon root keys and administrative credentials.
5. Bounded fee budgets and available liquidity.
6. Node availability and routing policy.
7. Payment, invoice, peer, balance, and topology privacy.
8. Node identity and external identities authenticated by LND signatures.

The primary desired security property for delegated applications is:

> A caller without value-egress authority cannot cause local principal to be
> paid to an unapproved address, invoice, peer, transaction output, or key,
> even by composing all RPCs and request fields available to that caller.

Secondary properties must be stated independently:

* The caller cannot burn more than a configured fee budget.
* The caller cannot make channel state unrecoverable.
* The caller cannot extend or delegate its own authority.
* The caller cannot observe data outside its application scope.
* The caller cannot cause unbounded downtime or liquidity lockup.

A feature that prevents redirection but permits force closes may satisfy the
first property while explicitly not satisfying the availability or fee-budget
properties.

## Trust boundaries and attacker classes

Security claims should name the strongest attacker they cover.

### T0: Network attacker without credentials

The attacker can reach an RPC or peer listener and can record, replay, delay,
or modify network traffic, but has no trusted credential and no TLS private
key.

### T1: Offline artifact thief

The attacker obtains one or more copied files, such as a mnemonic backup,
database snapshot, disk image, SCB, macaroon, TLS key, or password file. The
attacker does not control the live host.

### T2: Delegated credential attacker

The attacker obtains a subset of the node's macaroons. They can make arbitrary
and repeated requests, choose every request field, control Bitcoin addresses,
invoices and Lightning peers, and compose every RPC reachable with the stolen
credentials.

Permissions on two separate macaroons do not combine to satisfy one RPC check.
The attacker can still use the credentials in sequence to build a multi-RPC
attack.

### T3: Application compromise

The attacker controls an application process or operating-system account that
legitimately holds a delegated macaroon. It may also read that application's
configuration, logs, memory, TLS material, and network access.

### T4: LND service-account compromise

The attacker can execute code as the account running `lnd`, or can replace
files that the service account will execute or load. This is normally enough
to reach live wallet operations or persist until the next unlock.

### T5: Root, kernel, hypervisor, or provider compromise

The attacker controls the operating system, kernel, VM host, or equivalent
cloud-provider plane. They can modify the binary and configuration, inspect or
alter process memory, capture unlock material, replace policy, and persist.

Macaroons and same-host configuration are not intended to resist T5.

### T6: Signing-domain compromise

The attacker controls either the watch-only front end, the remote signer, or
the connection and credential between them. Each variant has a different
result and must not be described merely as "using a remote signer."

## Quick conclusions

The cases below support these short answers:

* A stolen aezeed without a custom passphrase exposes the deterministic seed.
  A weak passphrase creates an offline guessing problem. A strong passphrase
  can protect the stolen words, but does not protect a live unlocked wallet.
* A stolen wallet password is distinct from the aezeed passphrase. Combined
  with the databases, it exposes wallet keys and macaroon root keys. Combined
  with a reachable locked WalletUnlocker, it can be changed for an admin
  macaroon.
* An unlocked wallet does not, by itself, let a credential-less network
  attacker bake macaroons.
* A limited macaroon cannot bake more macaroons unless it includes
  `macaroon:generate`, the exact `BakeMacaroon` URI, or another path to the
  root key.
* A caller that can invoke `BakeMacaroon` can currently request unrelated
  permissions. That authority is administrative, not safely delegatable.
* A subset of stolen macaroons grants the union of RPC sequences reachable
  with those credentials. Separate macaroons do not combine within one RPC
  check, but they can be used in a multi-step attack.
* Default `router.macaroon`, `walletkit.macaroon`, and `signer.macaroon`
  credentials carry direct payment, spend, or signing risk.
* Stateless initialization removes default plaintext macaroon files from the
  LND host when used consistently. It does not protect an unlocked process.
* A compromised application can be contained by least-privilege credentials.
  A compromised LND service account, root account, kernel, or provider cannot
  be contained by same-host macaroon or configuration policy.
* A remote signer protects against seed extraction from the watch-only front
  end. It protects against unauthorized signing only if it independently
  enforces the intended transaction and channel policy.

## Case analysis: recovery and encryption secrets

### R1: Aezeed mnemonic stolen without a custom passphrase

**Assumption:** The attacker has all 24 words. The operator did not configure a
cipher seed passphrase.

**Current result:** The known default passphrase is used, so the attacker can
decrypt the mnemonic without guessing. The deterministic private keys and node
identity are permanently compromised. On-chain wallet funds can be recovered
and redirected. Funds from channels that peers later force close are also at
risk because the relevant keys are deterministic.

The mnemonic alone does not recreate the latest channel state. Proactively
finding peers and initiating recovery normally also needs an SCB or other
channel information.

**Preventable:** No LND permission or host control can repair seed secrecy after
disclosure. Funds can only be moved to a new seed and the old node identity and
channels retired.

**Mitigation:** Use a high-entropy cipher seed passphrase stored separately,
then follow a full seed-compromise recovery plan if the words are exposed.

### R2: Aezeed mnemonic stolen with a weak passphrase

**Assumption:** The attacker has all 24 words and the passphrase is derived
from a name, short phrase, reused password, or other guessable source.

**Current result:** The mnemonic contains the KDF salt and an authentication
check, so the attacker can perform offline guesses and recognize a correct
one. Aezeed version zero uses fixed scrypt parameters. There is no online rate
limit or alert for this attack.

Once guessed, the result is the same as R1.

**Preventable:** Online RPC controls, macaroon restrictions, and wallet lock
state do not affect offline guessing.

**Mitigation:** Use a randomly generated, independently stored passphrase with
enough entropy to resist offline guessing. A minimum character count alone is
not a strength guarantee.

### R3: Aezeed mnemonic stolen with a strong passphrase

**Assumption:** The attacker has the mnemonic but not a high-entropy,
independently stored passphrase.

**Current result:** The attacker is limited to offline guessing. The encrypted
mnemonic does not reveal the wallet password or macaroon root keys.

**Preventable:** A sufficiently strong passphrase can make recovery of the seed
economically infeasible for the modeled attacker.

**Cannot prevent:** It provides no protection if the passphrase is later
stolen, logged, entered into a compromised recovery machine, or recovered from
another copy. It also does not protect an already unlocked live wallet.

### R4: Cipher seed passphrase stolen without the mnemonic

**Assumption:** The attacker has only the cipher seed passphrase.

**Current result:** The passphrase alone does not derive keys and does not
unlock `wallet.db`. It becomes severe if any mnemonic copy is later obtained.

**Mitigation:** Store the mnemonic and passphrase in separate failure domains.
Rotate to a new seed if both are believed to have been exposed; merely changing
how a backup copy is encrypted does not change already derived keys.

### R5: Aezeed mnemonic and SCB stolen

**Assumption:** The attacker can decrypt the mnemonic and also has a current
`channel.backup` or exported SCBs.

**Current result:** The attacker has the keys and peer/channel recovery
information. They can create a clone, initiate data-loss recovery, and ask
peers to force close channels. The original and clone must never operate
concurrently.

**Preventable:** Macaroons on the original node do not protect an independently
restored clone.

**Mitigation:** Treat the combination as a severe seed compromise. Stop the
original node, recover under controlled conditions, move funds, and replace
the node identity and channels.

### R6: Wallet database stolen without the wallet password

**Assumption:** The attacker obtains `wallet.db`, but no wallet password and no
live process access.

**Current result:** Private wallet material remains encrypted and the attacker
can attempt offline password guesses. LND requires at least eight password
bytes, but this is a syntax rule, not a meaningful entropy guarantee.

**Preventable:** A strong, unique wallet password protects a powered-off
database snapshot against the modeled offline attacker.

**Cannot prevent:** It does not protect against a weak password, an auto-unlock
secret obtained with the database, or compromise while the wallet is unlocked.

### R7: Wallet password stolen without database or endpoint access

**Assumption:** The attacker has only the wallet password.

**Current result:** The password does not reveal the aezeed mnemonic by itself
and is not an RPC macaroon. It becomes administrative authority when combined
with the databases or access to the locked WalletUnlocker service.

**Mitigation:** Treat wallet-password disclosure as urgent even if no database
copy is known. Change it while the wallet is locked and rotate the macaroon
root keys if macaroon database exposure is possible.

### R8: Wallet password and locked WalletUnlocker endpoint

**Assumption:** The attacker knows the wallet password and can reach the RPC
server while the wallet is locked.

**Current result:** `UnlockWallet` is intentionally available without a
macaroon while locked. It unlocks the node but does not return an admin
macaroon. More critically, `ChangePassword` can authenticate with the current
wallet password, replace it, unlock the wallet, and return an admin macaroon.
The wallet password must therefore be treated as an administrative credential
for the locked service.

**Preventable:** A firewall or private unlock channel can prevent a remote
attacker with only the password from reaching the service.

**Cannot prevent:** Macaroon restrictions do not protect the pre-unlock RPC
surface because the wallet unlocker exists before macaroon authentication is
available.

### R9: Auto-unlock password file stolen

**Assumption:** The attacker reads the configured
`wallet-unlock-password-file`.

**Current result:** This is R7 and often also R8. If the attacker can obtain the
wallet and macaroon databases, the auto-unlock file removes their at-rest
password barrier. Storing the file beside the databases mainly provides
automation, not an additional security boundary.

**Mitigation:** Obtain the password at startup from a separate secret store or
operator-controlled channel. Limit file, process, backup, and provider access.
This still cannot protect a live process from T4 or T5.

### R10: Full data-directory snapshot

**Assumption:** The attacker obtains a copy of the LND data directory while the
live host remains trusted.

**Current result:** In the default state, the snapshot may contain plaintext
bearer macaroon files. A copied admin macaroon can control the live node once
it is unlocked and reachable; no wallet-password guess is necessary. The
snapshot also contains encrypted wallet and macaroon databases, channel state,
SCBs, TLS material, logs, and operational metadata.

**Mitigation:** Use stateless initialization, restrictive file permissions,
encrypted and access-controlled backups, separate root key IDs, and immediate
root-key revocation after suspected snapshot exposure.

Disk encryption protects a powered-off disk, but not a provider snapshot or a
snapshot taken after the volume is mounted.

### R11: `--noseedbackup` uses known default wallet passwords

**Assumption:** A funded node is started with `--noseedbackup` instead of a
recoverable aezeed wallet.

**Current result:** This mode uses known default internal wallet passwords. A
stolen wallet database therefore has no meaningful password-guessing barrier.
The operator also has no aezeed backup for ordinary disaster recovery, so loss
of the database can make funds unrecoverable.

**Mitigation:** Do not use `--noseedbackup` for a funded mainnet node. In a
normal aezeed wallet, LND requires a wallet password of at least eight bytes;
there is no supported "unset" wallet password mode equivalent to an empty
password.

## Case analysis: wallet runtime state

### W1: Wallet locked, network attacker has no wallet password

**Assumption:** The attacker can reach the WalletUnlocker RPC but has no
credential or local access.

**Current result:** The attacker can probe and attempt passwords and may cause
resource consumption, but cannot invoke normal wallet RPCs. Password checking
uses a KDF, which slows guesses but can also make unauthenticated attempts
expensive for the server.

**Mitigation:** Keep unlock RPC access private, monitor repeated failures, and
consider explicit rate limits and backoff as defense in depth.

### W2: Wallet always unlocked, network attacker has no macaroon

**Assumption:** The wallet and macaroon store are unlocked in the LND process,
but the attacker has only network reachability.

**Current result:** Unlock state alone does not authorize RPC access. The
attacker cannot ask LND to bake a macaroon without a credential that authorizes
`BakeMacaroon`, and cannot directly read the in-memory macaroon root key.

**Preventable:** TLS, macaroon verification, listener binding, and firewalling
remain meaningful against this attacker.

**Cannot prevent:** Always-unlocked operation increases the consequence of a
later process, service-account, kernel, or provider compromise.

### W3: Wallet unlocked, limited macaroon stolen

**Assumption:** The attacker has a macaroon without `macaroon:generate` and
without the exact `BakeMacaroon` URI permission.

**Current result:** The attacker cannot bake more macaroons merely because the
wallet is unlocked. They can exercise all methods and request fields authorized
by the stolen macaroon. Current permissions are method-level and generally do
not restrict amount, destination, fee, peer, channel point, or output script.

**Mitigation:** Scope the macaroon to exact RPCs, add persistent timeout and IP
caveats where useful, use a unique root key ID, and implement request-aware
policy for value-moving methods.

### W4: Wallet unlocked, macaroon-baking authority stolen

**Assumption:** The attacker has `macaroon:generate` or
`uri:/lnrpc.Lightning/BakeMacaroon`.

**Current result:** `BakeMacaroon` validates that requested permission names
exist, but does not restrict the new macaroon to the caller's current
authority. The attacker can request unrelated write, signer, or URI
permissions. This is authorization end game and should be treated as
administrative authority.

**Mitigation:** Never delegate `BakeMacaroon` under the current model. A future
implementation should require minted authority to be a subset of the caller's
authority and should handle request-aware restrictions without allowing a URI
permission to bypass them.

### W5: Wallet unlocked, LND service account compromised

**Assumption:** The attacker can execute code as the LND account or modify
files that account will execute or load.

**Current result:** At-rest encryption and stateless macaroon files are no
longer reliable boundaries. Depending on operating-system isolation, the
attacker can access process state, replace configuration or binaries, control
RPC sockets, wait for secrets, or invoke live signing paths. Redirect impact
should be assumed.

**Mitigation:** A dedicated unprivileged account, read-only executable paths,
mandatory access controls, process hardening, and separation from application
accounts reduce how an application compromise reaches W5. A policy-enforcing
signer in a separate trust domain is required to preserve a value policy after
W5.

### W6: Root, kernel, hypervisor, or provider compromised

**Assumption:** T5.

**Current result:** Same-host macaroon, password, configuration allowlist, disk
encryption, process, and audit policies can be changed or observed. The
attacker can replace LND before the next unlock or manipulate a currently
unlocked process. This is host-policy end game.

**Mitigation:** Move signing and enforceable value policy to a separately
administered machine or hardware boundary. Remote backups and monitoring help
recovery and detection but do not prevent unauthorized live signing.

## Case analysis: macaroon exposure

### M1: No macaroon, root key, or wallet password exposed

**Assumption:** T0 against a correctly configured, authenticated RPC server.

**Current result:** The attacker cannot call normal LND RPCs. Public RPC
exposure still creates a parsing, denial-of-service, and implementation-bug
surface.

**Mitigation:** Bind RPC listeners to loopback, a Unix socket, or a private
network where possible. Continue using TLS and macaroons on private networks.

### M2: Read-only macaroon exposed

**Assumption:** The attacker obtains the default read-only macaroon or an
equivalent set of read permissions.

**Current result:** Direct redirection is not intended. The attacker can learn
sensitive balances, transactions, invoices, payments, peers, channels, graph
data, addresses, macaroon IDs, and encrypted channel backups available through
read methods. Some invoice responses may contain preimages or application
metadata.

**Mitigation:** Treat confidentiality as a real boundary. Bake application
macaroons from exact read RPCs instead of using the broad default read-only
credential.

### M3: Invoice macaroon exposed

**Assumption:** The attacker obtains invoice read/write authority and the
additional address/on-chain read authority present in the default invoice
macaroon.

**Current result:** The attacker should not be able to spend existing wallet or
channel balances directly. They can observe invoice and on-chain data, create
invoices, interfere with receivables, cancel or settle hold invoices where the
RPC inputs permit it, and consume address state.

**Mitigation:** Separate invoice creation, lookup, subscription, hold-invoice
settlement, and cancellation into exact-RPC credentials. Treat the ability to
change receivables as financial integrity authority even when it cannot spend
existing principal.

### M4: Router macaroon or `offchain:write` exposed

**Assumption:** The attacker obtains the default router macaroon or broad
`offchain:write` authority.

**Current result:** The attacker can call `SendPaymentV2` and `SendToRouteV2`
and direct channel balance to an attacker-controlled Lightning destination.
There is no native macaroon caveat for maximum amount, cumulative budget,
payee, route, or fee. The same permission also authorizes unrelated routing
state and interception methods.

**Mitigation:** Do not treat `router.macaroon` as a routing-observation
credential. Payment applications need request-aware amount, destination,
cumulative, rate, and fee policies.

### M5: WalletKit macaroon or `onchain:write` exposed

**Assumption:** The attacker obtains the default WalletKit macaroon or broad
`onchain:write` authority.

**Current result:** The attacker can access direct spend and signing methods,
including `SendCoins`, `SendMany`, `SendOutputs`, `SignPsbt`, and
`FinalizePsbt`, along with transaction publication, UTXO leasing, fee bumping,
and wallet mutation. Arbitrary on-chain value redirection should be assumed.

The broad permission also demonstrates why the current `CloseChannel`
permission set cannot express a safe channel operator: a macaroon satisfying
its `onchain:write` requirement can already call `SendCoins`.

**Mitigation:** Split wallet send, PSBT fund, PSBT sign, publish, fee bump,
lease, import, and channel lifecycle authority. Add output, amount, account,
change, fee, and cumulative-budget policies.

### M6: Signer macaroon or `signer:generate` exposed

**Assumption:** The attacker obtains raw signer authority.

**Current result:** The signer service intentionally exposes raw transaction,
message, shared-key, and MuSig2 operations. This can authorize attacker-chosen
cryptographic operations with LND keys and may enable value redirection or
identity abuse depending on the available key locators and transaction data.

**Mitigation:** Treat the signer macaroon as key-use authority, not as a narrow
implementation detail. Restrict network access and require a policy-aware
signer for a strong destination or amount guarantee.

### M7: Exact URI macaroon exposed

**Assumption:** The macaroon contains only `uri:/package.Service/Method` for
one or more methods.

**Current result:** This is narrower than a broad entity permission, but it
authorizes every request field accepted by the method. For example:

* `uri:/lnrpc.Lightning/SendCoins` permits arbitrary supported destinations,
  amounts, selected outpoints, fee choices, and `send_all`.
* `uri:/lnrpc.Lightning/CloseChannel` permits arbitrary channel points,
  cooperative delivery addresses, force close, and caller-selected fee fields.
* `uri:/lnrpc.Lightning/OpenChannel` permits a caller-selected peer, funding
  amount, `push_sat`, `fund_max`, close address, and fee fields.
* `uri:/lnrpc.Lightning/BakeMacaroon` reaches the escalation described in W4.

Current authorization first tries the method's broad operations and then
accepts the exact URI as an alternative. An exact URI can therefore bypass new
operations added only to the static method permission map.

**Mitigation:** Supplemental request authorization must run regardless of how
the base method was authorized. URI permission must never mean "skip field
policy."

### M8: Multiple limited macaroons exposed

**Assumption:** The attacker obtains a subset of credentials from several
applications.

**Current result:** LND expects one macaroon per request, so permissions from
separate macaroons do not merge to satisfy a multi-operation check on one RPC.
The attacker can nevertheless chain calls authorized by each credential. For
example, a channel credential can force close funds back on-chain and a wallet
credential can later send them away.

**Mitigation:** Evaluate the union of reachable RPC sequences during threat
analysis. Isolate applications by operating-system account, network path, root
key ID, and signer policy so one compromise does not automatically collect all
credentials.

### M9: Macaroon root key exposed

**Assumption:** The attacker obtains a plaintext macaroon root key.

**Current result:** The attacker can bake valid macaroons offline with any
permission, without reaching `BakeMacaroon` and without knowing the wallet
password. Caveats on previously issued macaroons do not constrain macaroons
minted directly from the root key.

**Mitigation:** Revoke the affected non-default root key ID. If root key ID
zero or the scope of exposure is uncertain, generate new macaroon root keys and
redistribute all required credentials.

### M10: Encrypted `macaroons.db` exposed

**Assumption:** The attacker has the database but no wallet password or live
process access.

**Current result:** Root keys are encrypted using a key derived from the wallet
password. The attacker can guess the password offline. If it is recovered, the
attacker can decrypt root keys and mint arbitrary credentials.

**Mitigation:** Use a strong wallet password, protect database backups, rotate
root keys after combined password/database exposure, and do not assume
stateless initialization makes the root-key database disappear.

### M11: Default macaroon file exposed

**Assumption:** The attacker reads one of the default `*.macaroon` files.

**Current result:** Macaroons are bearer credentials. Deleting the copied file
does not revoke it. Several currently generated macaroon files use mode `0644`
and the admin file uses `0640`, so local account and group membership are part
of the boundary.

`lncli` normally adds a short timeout caveat to the copy it sends for a call.
That does not add a permanent timeout to the underlying file. An attacker who
copies the base file can use it with another client.

**Mitigation:** Prefer stateless initialization, mode `0600`, dedicated
application users, persistent expiry caveats, and a unique nonzero root key ID
per application.

### M12: Stateless initialization used correctly

**Assumption:** `create`, every `unlock`, and `changepassword` use
`--stateless_init`, and old on-disk macaroon files have been invalidated and
removed.

**Current result:** No default plaintext macaroon files need to remain on the
LND host. The encrypted root-key database still exists and is unlocked in the
running process. Delegated clients still hold bearer macaroons elsewhere.

**Prevents:** A read-only snapshot of the LND data directory no longer
automatically contains an admin macaroon file.

**Cannot prevent:** It does not resist service-account or root compromise,
process-memory access, stolen client credentials, or offline password guessing
against the databases.

### M13: Stateless initialization omitted during a later unlock

**Assumption:** The node was intended to be stateless, but a later unlock does
not set `--stateless_init`.

**Current result:** LND can regenerate default plaintext macaroon files. The
security property depends on an operator remembering a flag on every unlock.

**Mitigation:** Current operators must consistently set the flag and monitor
for generated files. A future design should persist stateless intent and fail
closed rather than silently weakening the deployment.

### M14: Macaroon revoked by root key ID

**Assumption:** A credential uses a unique nonzero root key ID and compromise
is detected.

**Current result:** Deleting that root key ID invalidates every macaroon minted
under it. Individual macaroons under the same ID are not independently tracked
or revoked. Root key ID zero cannot be deleted through `DeleteMacaroonID`.

**Mitigation:** Use a distinct nonzero root key ID for each application or
device. Avoid grouping unrelated credentials under ID zero. Rotation remains
necessary when the default ID is involved.

### M15: Timeout or IP caveat is present

**Assumption:** The stolen macaroon has a persistent expiry or source-address
caveat.

**Current result:** The caveat limits when or from where the credential can be
used. It does not restrict request fields, cumulative use, destination, or
amount. Source-address checks use the address observed by LND; a proxy, VPN,
container bridge, or SSH tunnel can make many clients appear identical.

**Mitigation:** Use these caveats as exposure reducers, not as substitutes for
least privilege or request policy. Test the observed address in the real
deployment path.

### M16: Admin macaroon exposed

**Assumption:** The attacker obtains `admin.macaroon` or an equivalent custom
credential containing all current read and write operations.

**Current result:** This is RPC authorization end game. The attacker can spend
funds, make payments, sign, change operational state, mint more macaroons, and
revoke non-default root key IDs while the node is unlocked and reachable.

The admin macaroon does not directly reveal the aezeed or reconstruct missing
channel state. It provides enough live authority that incident response must
assume funds and private application data are at risk.

**Mitigation:** Immediately remove RPC reachability, preserve evidence, rotate
all macaroon root keys, audit value-moving and signing activity, and move funds
if unauthorized use cannot be excluded. Deleting the file alone is not
revocation.

## Case analysis: transport and deployment

### D1: TLS certificate exposed

**Assumption:** The attacker obtains only `tls.cert`.

**Current result:** The certificate is public information and does not
authorize client calls. Clients use it to authenticate the server.

### D2: TLS private key exposed

**Assumption:** The attacker obtains `tls.key` and can intercept or redirect a
client connection.

**Current result:** The attacker can impersonate the RPC server to clients that
trust that certificate and may capture macaroons or sensitive requests. The
TLS key alone does not authenticate the attacker as an RPC client to the real
LND server.

**Mitigation:** Rotate TLS material and redistribute the pinned certificate.
Use `--tlsencryptkey` to protect the persistent key at rest, while recognizing
that it must be available to the live process after unlock.

### D3: Macaroon exposed through transport or logs

**Assumption:** A reverse proxy, TLS terminator, debugger, access log, crash
report, shell history, or monitoring system records the macaroon metadata.

**Current result:** Any recorded base credential can be replayed while its
caveats permit. TLS between the client and proxy does not protect the token
from the proxy itself.

**Mitigation:** Never log the macaroon header or hex value. Minimize TLS
termination points, sanitize diagnostics, use short-lived derived credentials,
and separate application root key IDs.

### D4: `--no-macaroons` is enabled

**Assumption:** LND accepts RPC calls without macaroon verification on an
allowed private listener.

**Current result:** Any process or network participant that can reach the RPC
listener has the authority of the exposed RPC surface. A "private" network is
not an authorization boundary against another workload, compromised router,
SSRF, container, or local user.

**Mitigation:** Do not use `--no-macaroons` for a funded node. Private network
binding and macaroon authentication should be layered, not substituted.

### D5: Third-party application account compromised

**Assumption:** T3, while the LND service account and host remain trusted.

**Current result:** The attacker's authority should be limited to the
application's macaroons, endpoint reachability, and any application data. This
is the primary case that least-privilege macaroons can meaningfully contain.

**Mitigation:** Give each application its own OS account, exact-RPC macaroon,
nonzero root key ID, network path, expiry strategy, and audit identity. Do not
mount the LND data directory or share the LND service account.

### D6: LND configuration is modified

**Assumption:** The attacker can persistently change `lnd.conf` or startup
arguments and cause a restart.

**Current result:** Configuration-based destination allowlists, mandatory
middleware, listener restrictions, signer endpoints, and authentication modes
can be changed or disabled. The security result depends on who controls the
executable and service definition as well as the file itself.

**Mitigation:** Make configuration and service definitions read-only to the
LND account where practical, monitor their integrity, and place strong value
policy outside this trust domain if configuration compromise is in scope.

### D7: Hosting provider or VM administrator compromised

**Assumption:** The provider can snapshot disks and memory, replace boot media,
or inspect the VM.

**Current result:** This is T5, even if the guest OS is fully patched and disk
encryption is enabled. Provider-controlled boot and memory access can wait for
the wallet to unlock.

**Mitigation:** A provider-independent signer or hardware policy boundary can
reduce key and signing exposure. Provider diversity for backups and monitoring
improves recovery and detection but does not secure live same-provider signing.

## Case analysis: remote signing

### S1: Watch-only front end compromised, signer remains trusted

**Assumption:** The public LND instance has no private wallet keys, but the
attacker controls it and obtains the credential and network path used to reach
the remote signer.

**Current result:** Remote signing prevents straightforward extraction of the
seed from the front end. It does not automatically prevent unauthorized
signing. The documented signer credential includes broad signer and on-chain
write permissions. A compromised front end can make attacker-chosen requests
that the signer API accepts, including raw signing or PSBT operations.

**Mitigation:** Restrict the signer connection and credential, but do not claim
a value policy unless the signer independently validates the complete
transaction or protocol context, destinations, amounts, fees, channel state,
and replay rules.

### S2: Remote signer compromised

**Assumption:** The attacker controls the signer process, its wallet, or its
host.

**Current result:** Private key secrecy and signature integrity are lost. The
attacker may also withhold required signatures and disrupt the watch-only node.
The watch-only split does not protect against compromise of the private-key
domain.

**Mitigation:** Harden, isolate, monitor, and minimize the signer. Recovery
requires moving funds and replacing compromised keys, subject to current
channel-state constraints.

### S3: Policy-enforcing signer remains trusted

**Assumption:** The public node and its configuration are compromised, but a
separately administered signer receives enough authenticated context to enforce
a value policy and refuses requests outside it.

**Desired result:** Seed extraction and unauthorized value redirection remain
prevented. Availability attacks, channel force closes, and requests that fit
the signer's policy may remain possible.

**Design requirement:** The signer must parse and validate what it signs. Key
separation without semantic validation does not satisfy this case.

## Case analysis: channel state, peers, and recovery

### C1: SCB stolen without the seed

**Assumption:** The attacker has `channel.backup` but no seed or live node
credential.

**Current result:** The SCB is encrypted to the node and cannot be used alone
to sign or recover funds. It should still be treated as sensitive operational
data and protected against deletion or rollback.

### C2: Current channel database stolen without keys

**Assumption:** The attacker has `channel.db`, but not the seed, wallet private
keys, or a live signing path.

**Current result:** The database contains sensitive channel and peer state but
does not independently provide deterministic private keys. It increases the
impact of any later key compromise.

**Mitigation:** Encrypt and access-control backups and snapshots. Do not expose
channel state to application accounts.

### C3: Stale channel database restored or clone started

**Assumption:** An operator or attacker starts LND from stale channel state, or
runs two instances sharing the same seed and identity.

**Current result:** The stale node can use revoked commitment state, conflict
with the live node, trigger data-loss handling, or create conditions that risk
funds. This is a state-consistency failure, not a macaroon failure.

**Mitigation:** Never run two nodes with the same seed. Stop the original before
recovery or migration. Use SCB recovery for catastrophic loss and follow the
documented migration procedure for planned moves.

### C4: Malicious peer while LND and keys remain trusted

**Assumption:** A channel peer deviates from the protocol, force closes, holds
HTLCs, attempts a revoked-state breach, or causes liquidity grief.

**Current result:** Protocol validation, the breach arbiter, chain monitoring,
and optional watchtowers address different parts of this threat. Macaroons do
not constrain the peer.

**Mitigation:** Maintain reliable chain access and backups and use an
independent watchtower where appropriate. A watchtower helps when a peer
broadcasts revoked state while the node is offline; it does not prevent RPC
credential theft or a malicious local close destination.

### C5: Chain backend censored, eclipsed, or malicious

**Assumption:** LND receives delayed or misleading chain visibility or fee
information.

**Current result:** The attacker may delay confirmation and breach detection,
distort fee selection, or disrupt operation. This can combine with a malicious
peer to threaten channel funds.

**Mitigation:** Harden and monitor the chain backend, use independent chain
visibility and watchtowers, and bound caller-selected fee policies. This is
separate from RPC authentication.

## Security-sensitive RPC classes

The current entity/action permission model groups methods with very different
impact. The list below is intentionally based on attacker outcomes rather than
package ownership.

At the entity/action level, the most important current consequences are:

* `onchain:write` includes direct send, PSBT signing and finalization, fee,
  lease, wallet mutation, and part of channel lifecycle authority.
* `offchain:write` includes direct Lightning payment, routing mutation, HTLC
  interception, payment-record deletion, watchtower-client mutation, channel
  lifecycle, and backup restoration authority.
* `signer:generate` exercises raw signing, message, shared-key, and MuSig2 key
  operations.
* `macaroon:generate` can mint unrelated permissions under the current model.
* `macaroon:write` can revoke non-default root key IDs and register RPC
  middleware.
* `invoices:write` changes invoices and receivables but does not currently
  authorize wallet or Lightning sends by itself.
* `info:write`, `peers:write`, `address:write`, and `message:write` affect
  availability, network identity, address state, or external authentication,
  but do not currently authorize a direct send by themselves.
* Read permissions can expose substantial financial, peer, payment, invoice,
  address, backup, and operational metadata even when they cannot redirect
  principal.

### Direct value redirection

* `Lightning.SendCoins`
* `Lightning.SendMany`
* `WalletKit.SendOutputs`
* `Router.SendPaymentV2`
* `Router.SendToRouteV2`
* `WalletKit.SignPsbt`
* `WalletKit.FinalizePsbt`
* `Signer.SignOutputRaw`
* `Signer.ComputeInputScript`

These methods need destination, amount, output, account, fee, and cumulative
budget policy. An exact URI alone is not a safe spending policy.

### Conditional value redirection

* `Lightning.CloseChannel` can redirect local cooperative-close funds through
  `delivery_address`.
* `Lightning.OpenChannel` and `OpenChannelSync` can transfer value through
  `push_sat`, commit all wallet funds through `fund_max`, and establish an
  external cooperative-close destination through `close_address`.
* `Lightning.BatchOpenChannel` exposes the corresponding risks for every
  channel in the batch.
* `WalletKit.FundPsbt` selects and locks wallet inputs for caller-selected
  outputs. It becomes a complete spend path when combined with signing.
* `WalletKit.PublishTransaction` broadcasts supplied signed transactions. It
  is not a signing capability alone but completes other attack paths.

These methods need field-sensitive supplemental authorization that cannot be
bypassed with a URI macaroon.

### Credential and policy administration

* `Lightning.BakeMacaroon`
* `Lightning.DeleteMacaroonID`
* `RegisterRPCMiddleware`

Credential minting should be monotonic: delegated credentials must not mint
authority they do not already possess. Middleware registration and root-key
revocation can change authorization or availability and should be treated as
administrative operations.

### Signing and identity use

* Raw transaction and MuSig2 signer methods.
* Message signing methods.
* Shared-key derivation methods.

These may not immediately move Bitcoin in every request, but they exercise
private-key authority and can affect external authentication or protocols.

### Financial destruction and liquidity griefing

* `WalletKit.BumpFee` and `BumpForceCloseFee`
* Caller-selected fees on send, open, and close methods
* `WalletKit.LeaseOutput`
* `Lightning.AbandonChannel`
* `Lightning.FundingStateStep`
* `Lightning.RestoreChannelBackups`
* `Router.HtlcInterceptor`

These need bounded fees, lease duration, state-transition, channel-point, and
recovery policy even when they cannot pay the attacker directly.

### Availability and policy integrity

* `Lightning.StopDaemon`
* `Lightning.UpdateChannelPolicy`
* Peer connect and disconnect methods
* `Lightning.ChannelAcceptor`
* Router mission-control and channel-status mutation methods
* Invoice cancellation, settlement, and deletion methods

Availability, routing revenue, receivables, and accounting integrity are
separate assets from principal and should receive separate permissions.

## Cross-case attack paths

Reviewing one RPC or secret in isolation misses common compositions.

### A1: Broad channel operator becomes wallet spender

1. An application receives `onchain:write` and `offchain:write` because those
   are the current `CloseChannel` requirements.
2. Its macaroon is stolen.
3. The attacker calls `SendCoins` using `onchain:write`, without calling
   `CloseChannel` at all.

A close-address allowlist does not prevent this path. Channel lifecycle
authority must be separated from wallet spending authority.

### A2: Close-only URI redirects cooperative-close funds

1. An application receives `uri:/lnrpc.Lightning/CloseChannel`.
2. The attacker supplies an external `delivery_address`.
3. Static operation checks are satisfied by the URI fallback.

Request policy must run after base method authorization and apply to every
authorization path.

### A3: Channel open establishes the later theft destination

1. The attacker obtains `OpenChannel` authority.
2. They open to a controlled peer and set `push_sat` or an attacker-controlled
   `close_address`.
3. Value is transferred immediately or the future cooperative-close
   destination is committed.

Protecting only `CloseChannel.delivery_address` is incomplete.

### A4: Router credential drains channel balance

1. A service receives the default router macaroon for routing automation.
2. The service is compromised.
3. The attacker pays their own invoice using `SendPaymentV2`.

Routing observation, routing policy, forwarding interception, and payment
authority must not share one write permission.

### A5: Stateless front end still controls an unrestricted signer

1. The public node is watch-only and contains no seed.
2. It holds a broad macaroon for the remote signer.
3. The public node is compromised.
4. The attacker asks the signer to sign an attacker-selected spend.

Key non-extraction is preserved, but value redirection is not. A semantic
signing policy is required.

### A6: Wallet password becomes admin macaroon

1. The attacker learns the wallet password.
2. They wait for or cause a state where the WalletUnlocker RPC is reachable.
3. They call `ChangePassword` with a new password.
4. The response returns an admin macaroon.

Wallet-password handling and unlock endpoint reachability are part of the
administrative credential boundary.

### A7: Snapshot bypasses database encryption

1. The attacker obtains a full default data-directory snapshot.
2. The snapshot contains `admin.macaroon` in plaintext.
3. The legitimate node remains reachable and later unlocks.
4. The copied macaroon controls the live node.

Strong database encryption does not compensate for plaintext bearer tokens in
the same snapshot.

## Control-to-threat mapping

No single control addresses all cases:

* A cipher seed passphrase primarily addresses R1 through R3.
* A wallet password primarily addresses offline `wallet.db` and
  `macaroons.db` exposure, and itself becomes an unlock credential in R8.
* Stateless initialization primarily addresses plaintext macaroon files in
  R10 and M11 through M13.
* Least-privilege macaroons primarily address T2 and T3.
* Unique root key IDs primarily reduce revocation scope after T2 or T3.
* Timeout and IP caveats reduce replay opportunity but do not enforce value
  policy.
* TLS and private listeners primarily address T0 and transport capture.
* Request-aware authorization addresses attacker-controlled RPC fields under
  T2 and T3.
* An upfront shutdown script constrains cooperative-close negotiation, but not
  arbitrary on-chain spending after a force close.
* A watchtower primarily addresses a malicious peer while the node is offline.
* A watch-only front end addresses private-key extraction from that machine,
  but needs a policy signer to address unauthorized signing.
* Backups address availability and recovery; they can increase attacker impact
  when combined with seed disclosure.

## Design requirements derived from the cases

### 1. Every feature names its covered cases

A security change must state:

* The strongest attacker class it resists.
* The protected asset and precise invariant.
* Accepted residual effects, such as force close, bounded fees, or downtime.
* The cases explicitly out of scope.

"More secure" is not an adequate claim.

### 2. Split permissions by outcome, not subsystem

At minimum, the model needs to distinguish:

* Channel open, cooperative close, force close, and policy management.
* On-chain send, send-all, PSBT funding, signing, publication, and fee bump.
* Lightning payment, routing observation, routing policy, and HTLC
  interception.
* Invoice creation, observation, settlement, cancellation, and deletion.
* Credential minting, credential revocation, and middleware administration.
* Message signing, transaction signing, and shared-key derivation.

### 3. Authorize request semantics

Security-sensitive methods need policy over:

* Destination addresses, scripts, invoices, payees, and peers.
* Amount per request and cumulative amount over time.
* Maximum on-chain and routing fees.
* `send_all`, `fund_max`, `push_sat`, and cooperative delivery fields.
* Channel points, accounts, selected outpoints, and change outputs.
* PSBT inputs, outputs, sighash modes, and publication.
* Lease duration and state-transition identifiers.

### 4. Make supplemental policy unavoidable

Broad operations, exact URI permissions, custom macaroons, REST, gRPC, and
streaming RPCs must all reach the same supplemental checks. Base method
authorization must not bypass field policy.

### 5. Make delegation monotonic

A delegated principal must not mint credentials with more authority than it
has. `BakeMacaroon` should either remain admin-only by definition or prove that
the requested operations and request policies are subsets of the caller's
authority.

### 6. Make value policy compositional

The authorization review must consider all credentials and all reachable RPC
sequences available to one application or host. A safe close permission is not
useful if the same principal can call an unrestricted send or signer method.

### 7. Improve credential lifecycle

* Generate files with mode `0600`.
* Persist stateless initialization intent and fail closed on later unlocks.
* Default application credentials to unique nonzero root key IDs.
* Associate IDs with names, creation times, expiry, purpose, and last use.
* Support narrow rotation and clear incident-response commands.
* Warn when a requested permission implies spend, signing, or credential
  administration.

### 8. Add bounded, attributable audit events

Security-sensitive calls should emit an audit identity, method, root key ID or
non-secret credential fingerprint, source, decision, and a redacted semantic
summary. Audit logs must never include bearer macaroons, seed material, wallet
passwords, preimages, or unnecessary private request data.

Remote or append-only audit storage can help T4 detection. Same-host logs are
not trustworthy after T5.

### 9. Treat the signer as a policy boundary only when it enforces policy

The signer protocol must carry enough authenticated context to validate the
complete effect of a signature. A signer that accepts raw requests from a
compromised front end is a key-separation boundary, not a value-policy
boundary.

### 10. Test attack chains, not only permission maps

Tests should attempt:

* Every direct send with attacker-controlled destinations and maximum values.
* Cooperative close through both open-time and close-time addresses.
* `push_sat`, `fund_max`, `send_all`, excessive fees, and selected outpoints.
* URI-only authorization bypasses.
* PSBT and raw-signer alternatives to normal send RPCs.
* Credential escalation through baking and middleware.
* Multi-macaroon, multi-RPC sequences.
* Revocation, rotation, stateless unlock regression, and proxy/IP behavior.

## Adding and reviewing cases

New cases should use the same explicit structure:

1. **Assumption:** List every secret, credential, host capability, endpoint,
   and external actor the attacker controls. Also list important capabilities
   they do not have.
2. **Current result:** Identify the strongest reachable Observe, Disrupt, Burn,
   Redirect, Authorize, and Recover impacts. Include multi-RPC compositions.
3. **Preventable:** Name the current or proposed trusted component that can
   still enforce a boundary under those assumptions.
4. **Cannot prevent:** State which requested guarantees are already outside the
   remaining trust boundary.
5. **Mitigation:** Reduce probability or impact without overstating prevention.
6. **Detection and recovery:** Identify useful audit events, credential
   rotation, shutdown, fund movement, or channel recovery procedures.
7. **Tests:** Exercise broad permissions, URI permissions, request fields,
   alternate RPC paths, multiple credentials, restarts, and revocation.

When one assumption changes, create or reference another case instead of
silently strengthening the attacker halfway through the analysis. In
particular, "server access" must distinguish filesystem read, application code
execution, LND service-account execution, root, and hypervisor control.

## Open policy questions

The following questions should be decided before implementing a replacement
for the current close-address work:

1. Is the primary guarantee theft prevention, all principal-loss prevention,
   or also availability and liquidity protection?
2. Is an attacker allowed to force close every channel and incur normal fees?
3. What server-side maximum fee loss is acceptable?
4. What proves an address is operator controlled: wallet derivation, upfront
   shutdown commitment, descriptor derivation, or an explicit allowlist?
5. How are one-time external cold-wallet addresses represented without address
   reuse?
6. Does a channel operator need open authority, close authority, or both?
7. May a channel operator use `push_sat`, `fund_max`, external funding shims,
   or externally supplied close addresses?
8. Should payment limits be per request, per time window, per payee, per
   application, or all of these?
9. Which policy is enforced by LND and which must be enforced by a separate
   signer or mandatory middleware?
10. How are existing broad and URI macaroons migrated without silently
    preserving bypasses?
11. Which compromise events automatically revoke credentials or stop signing?
12. Which audit signals are safe enough to export and useful enough to alert
    on?

Resolving these questions produces an explicit security contract. Permission
names and configuration fields should be chosen only after that contract is
agreed.

## Implementation references

The current behavior described here can be traced to:

* Aezeed encryption and fixed KDF parameters in
  [`aezeed/cipherseed.go`](../aezeed/cipherseed.go).
* Wallet initialization, unlock, password change, and stateless flags in
  [`walletunlocker/service.go`](../walletunlocker/service.go).
* Macaroon root-key encryption and revocation in
  [`macaroons/store.go`](../macaroons/store.go).
* Broad and exact URI authorization in
  [`macaroons/service.go`](../macaroons/service.go).
* Main RPC permissions and `BakeMacaroon` in
  [`rpcserver.go`](../rpcserver.go).
* Router permissions in
  [`lnrpc/routerrpc/router_server.go`](../lnrpc/routerrpc/router_server.go).
* WalletKit permissions in
  [`lnrpc/walletrpc/walletkit_server.go`](../lnrpc/walletrpc/walletkit_server.go).
* Signer permissions in
  [`lnrpc/signrpc/signer_server.go`](../lnrpc/signrpc/signer_server.go).
* Channel open and close request fields in
  [`lnrpc/lightning.proto`](../lnrpc/lightning.proto).
