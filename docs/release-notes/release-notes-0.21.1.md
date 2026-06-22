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
    - [Robustness](#robustness)
    - [Tooling and Documentation](#tooling-and-documentation)
- [Contributors (Alphabetical Order)](#contributors-alphabetical-order)

# Bug Fixes

* [Updated the `tor` module to
  `v1.1.7`](https://github.com/lightningnetwork/lnd/pull/10907) so fresh
  nodes started with `--tor.active --tor.v3` create v3 onion services with
  `NEW:ED25519-V3`. Previously, the root module still resolved `tor v1.1.6`,
  which could default new onion service creation to the retired v2
  `NEW:RSA1024` key type that modern Tor rejects with `513 Invalid key type`.

* [Fixed an issue](https://github.com/lightningnetwork/lnd/issues/10840)
  ([PR #10869](https://github.com/lightningnetwork/lnd/pull/10869))
  where an incoming HTLC resolver could treat a foreign spend of the
  commitment HTLC output as its own success transaction, causing a
  phantom second-level input to be offered to the sweeper. The resolver
  now validates the spending transaction's expected output before
  sweeping it.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

### ⚠️ **Warning:** Deprecated fields in `lnrpc.Hop` will be removed in release version **0.22**

### ⚠️ **Warning:** The deprecated fee rate option `--sat_per_byte` will be removed in release version **0.22**

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Robustness

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Ziggie
