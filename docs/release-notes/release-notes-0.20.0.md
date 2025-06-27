# Release Notes
- [Bug Fixes](#bug-fixes)
- [New Features](#new-features)
    - [Functional Enhancements](#functional-enhancements)
    - [RPC Additions](#rpc-additions)
    - [lncli Additions](#lncli-additions)
- [Improvements](#improvements)
    - [Functional Updates](#functional-updates)
    - [RPC Updates](#rpc-updates)
    - [lncli Updates](#lncli-updates)
    - [Breaking Changes](#breaking-changes)
    - [Performance Improvements](#performance-improvements)
    - [Deprecations](#deprecations)
- [Technical and Architectural Updates](#technical-and-architectural-updates)
    - [BOLT Spec Updates](#bolt-spec-updates)
    - [Testing](#testing)
    - [Database](#database)
    - [Code Health](#code-health)
    - [Tooling and Documentation](#tooling-and-documentation)

# Bug Fixes

- Fixed potential update inconsistencies in node announcements [by creating
  a shallow copy before modifications](
  https://github.com/lightningnetwork/lnd/pull/9815). This ensures the original
  announcement remains unchanged until the new one is fully signed and
  validated.

# New Features

## Functional Enhancements

## RPC Additions
* When querying [`ForwardingEvents`](https://github.com/lightningnetwork/lnd/pull/9813)
logs, the response now include the incoming and outgoing htlc indices of the payment 
circuit. The indices are only available for forwarding events saved after v0.20.


* The `lncli addinvoice --blind` command now has the option to include a
  chained channels [1](https://github.com/lightningnetwork/lnd/pull/9127)
  [2](https://github.com/lightningnetwork/lnd/pull/9925)
  incoming list `--blinded_path_incoming_channel_list` which gives users the 
  control of specifying the channels they prefer to receive the payment on. With
  the option to specify multiple channels this control can be extended to
  multiple hops leading to the node.


* The `lnrpc.ForwardingHistory` RPC method now supports filtering by 
  [`incoming_chan_ids` and `outgoing_chan_ids`](https://github.com/lightningnetwork/lnd/pull/9356). 
  This allows to retrieve forwarding events for specific channels.


* `DescribeGraph`, `GetNodeInfo`, `GetChanInfo` and the corresponding lncli
   commands [now have flag](https://github.com/lightningnetwork/lnd/pull/9950)
  `include_auth_proof`. With the flag, these APIs add AuthProof (signatures from
  the channel announcement) to the returned ChannelEdge.

* A [new config](https://github.com/lightningnetwork/lnd/pull/10001) value
  `--htlcswitch.quiescencetimeout` is added to allow specify the max duration
  the channel can be quiescent. A minimal value of 30s is enforced, and a
  default value of 60s is used. This value is used to limit the dependent
  protocols like dynamic commitments by restricting that the operation must
  finish under this timeout value. Consider using a larger timeout value if you
  have a slow network.


## lncli Additions

* [`lncli sendpayment` and `lncli queryroutes` now support the
  `--route_hints` flag](https://github.com/lightningnetwork/lnd/pull/9721) to
  support routing through private channels.


* The `lncli fwdinghistory` command now supports two new flags:
  [`--incoming_chan_ids` and `--outgoing_chan_ids`](https://github.com/lightningnetwork/lnd/pull/9356).
  These filters allows to query forwarding events for specific channels.

# Improvements
## Functional Updates

* Graph Store SQL implementation and migration project:
  * Introduce an [abstract graph 
    store](https://github.com/lightningnetwork/lnd/pull/9791) interface. 
  * Start [validating](https://github.com/lightningnetwork/lnd/pull/9787) that 
    byte blobs at the end of gossip messages are valid TLV streams.
  * Various [preparations](https://github.com/lightningnetwork/lnd/pull/9692) 
    of the graph code before the SQL implementation is added.
  * Only [fetch required 
    fields](https://github.com/lightningnetwork/lnd/pull/9923) during graph 
    cache population. 
  * Add graph schemas, queries and CRUD:
    * [1](https://github.com/lightningnetwork/lnd/pull/9866)
    * [2](https://github.com/lightningnetwork/lnd/pull/9869)
    * [3](https://github.com/lightningnetwork/lnd/pull/9887)
    * [4](https://github.com/lightningnetwork/lnd/pull/9931)
    * [5](https://github.com/lightningnetwork/lnd/pull/9935)
    * [6](https://github.com/lightningnetwork/lnd/pull/9936)
    * [7](https://github.com/lightningnetwork/lnd/pull/9937)
    * [8](https://github.com/lightningnetwork/lnd/pull/9938)
    * [9](https://github.com/lightningnetwork/lnd/pull/9939)
    * [10](https://github.com/lightningnetwork/lnd/pull/9971)
    * [11](https://github.com/lightningnetwork/lnd/pull/9972)

## RPC Updates

## lncli Updates

## Code Health

## Breaking Changes
## Performance Improvements

## Deprecations

### ⚠️ **Warning:** The following RPCs will be removed in release version **0.21**:

| Deprecated RPC Method | REST Equivalent | HTTP Method | Path | Replaced By |
|----------------------|----------------|-------------|------------------------------|------------------|
| [`lnrpc.SendToRoute`](https://lightning.engineering/api-docs/api/lnd/lightning/send-to-route/index.html) <br> [`routerrpc.SendToRoute`](https://lightning.engineering/api-docs/api/lnd/router/send-to-route/) | ❌ (No direct REST equivalent) | — | — | [`routerrpc.SendToRouteV2`](https://lightning.engineering/api-docs/api/lnd/router/send-to-route-v2/) |
| [`lnrpc.SendPayment`](https://lightning.engineering/api-docs/api/lnd/lightning/send-payment/) <br> [`routerrpc.SendPayment`](https://lightning.engineering/api-docs/api/lnd/router/send-payment/) | ✅ | `POST` | `/v1/channels/transaction-stream` | [`routerrpc.SendPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/send-payment-v2/index.html) |
| [`lnrpc.SendToRouteSync`](https://lightning.engineering/api-docs/api/lnd/lightning/send-to-route-sync/index.html) | ✅ | `POST` | `/v1/channels/transactions/route` | [`routerrpc.SendToRouteV2`](https://lightning.engineering/api-docs/api/lnd/router/send-to-route-v2/) |
| [`lnrpc.SendPaymentSync`](https://lightning.engineering/api-docs/api/lnd/lightning/send-payment-sync/index.html) | ✅ | `POST` | `/v1/channels/transactions` | [`routerrpc.SendPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/send-payment-v2/index.html) |
| [`router.TrackPayment`](https://lightning.engineering/api-docs/api/lnd/router/track-payment/index.html) | ❌ (No direct REST equivalent) | — | — | [`routerrpc.TrackPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/track-payment-v2/) |

🚨 **Users are strongly encouraged** to transition to the new **V2 methods** before release **0.21** to ensure compatibility:

| New RPC Method | REST Equivalent | HTTP Method | Path |
|---------------|----------------|-------------|------------------------|
| [`routerrpc.SendToRouteV2`](https://lightning.engineering/api-docs/api/lnd/router/send-to-route-v2/) | ✅ | `POST` | `/v2/router/route/send` |
| [`routerrpc.SendPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/send-payment-v2/index.html) | ✅ | `POST` | `/v2/router/send` |
| [`routerrpc.TrackPaymentV2`](https://lightning.engineering/api-docs/api/lnd/router/track-payment-v2/) | ✅ | `GET` | `/v2/router/track/{payment_hash}` |

# Technical and Architectural Updates
## BOLT Spec Updates

* Explicitly define the [inbound fee TLV 
  record](https://github.com/lightningnetwork/lnd/pull/9897) on the 
  `channel_update` message and handle it explicitly throughout the code base 
  instead of extracting it from the TLV stream at various call-sites.

## Testing

* Previously, automatic peer bootstrapping was disabled for simnet, signet and
  regtest networks even if the `--nobootstrap` flag was not set. This automatic
  disabling has now been 
  [removed](https://github.com/lightningnetwork/lnd/pull/9967) meaning that any 
  test network scripts that rely on bootstrapping being disabled will need to 
  explicitly define the `--nobootstrap` flag.

## Database

## Code Health

## Tooling and Documentation

* lntest: [enable neutrino testing with bitcoind](https://github.com/lightningnetwork/lnd/pull/9977)

# Contributors (Alphabetical Order)

* Abdulkbk
* Boris Nagaev
* Elle Mouton
* Funyug
* Mohamed Awnallah
* Pins
* Yong Yu
