package lnwallet

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// Sentinel errors returned by ChannelParams validation. They are wrapped with
// concrete values for logging, so callers must use errors.Is to match them.
var (
	// ErrDynParamsNoChange is returned when a dynamic-commitments parameter
	// proposal does not change any parameter.
	ErrDynParamsNoChange = errors.New("dynamic params proposal is empty")

	// ErrDynDustLimitTooSmall is returned when a proposed dust limit is
	// below the protocol minimum.
	ErrDynDustLimitTooSmall = errors.New("dust limit below minimum")

	// ErrDynDustLimitTooLarge is returned when a proposed dust limit is
	// above the maximum we consider safe.
	ErrDynDustLimitTooLarge = errors.New("dust limit above maximum")

	// ErrDynReserveBelowDust is returned when either peer's channel reserve
	// would end up below either peer's dust limit.
	ErrDynReserveBelowDust = errors.New(
		"channel reserve below dust limit",
	)

	// ErrDynReserveTooLarge is returned when a proposed channel reserve
	// exceeds the maximum fraction of the channel capacity we allow.
	ErrDynReserveTooLarge = errors.New(
		"channel reserve exceeds maximum",
	)

	// ErrDynCsvDelayTooLarge is returned when a proposed CSV delay
	// (to_self_delay) exceeds our configured maximum.
	ErrDynCsvDelayTooLarge = errors.New("csv delay exceeds maximum")

	// ErrDynMaxAcceptedTooLarge is returned when a proposed max_accepted_
	// htlcs value exceeds the protocol maximum.
	ErrDynMaxAcceptedTooLarge = errors.New(
		"max accepted htlcs exceeds protocol maximum",
	)

	// ErrDynHtlcMinTooLarge is returned when a proposed htlc_minimum_msat
	// exceeds the corresponding max_htlc_value_in_flight_msat.
	ErrDynHtlcMinTooLarge = errors.New(
		"htlc minimum exceeds max value in flight",
	)
)

// ChannelParams is the canonical, internal representation of a proposed change
// to the mutable ("dynamic") channel parameters. It is shared by the RPC, wire,
// wallet, and updater layers so they all speak the same param vocabulary, and
// it mirrors the lnwire.DynPropose message one-to-one.
//
// Each field is optional: an unset field means the corresponding parameter is
// left unchanged by the proposal. The fields carry the values the proposer
// advertised at channel open, with the same meaning as the like-named fields of
// open_channel/accept_channel.
//
// NOTE: Only DustLimit and CsvDelay are script/output-rendering parameters and
// therefore feed the commit-chain epoch history (so historical commitment and
// HTLC scripts stay reconstructable). All the other fields are pure policy
// parameters and MUST NOT create epoch entries.
type ChannelParams struct {
	// DustLimit is the sender's dust_limit_satoshis for its own commitment
	// transaction. It is an epoch-affecting (script-rendering) parameter.
	DustLimit fn.Option[btcutil.Amount]

	// MaxValueInFlight is the sender's max_htlc_value_in_flight_msat limit.
	// It is a policy parameter.
	MaxValueInFlight fn.Option[lnwire.MilliSatoshi]

	// HtlcMinimum is the sender's htlc_minimum_msat floor. It is a policy
	// parameter.
	HtlcMinimum fn.Option[lnwire.MilliSatoshi]

	// ChannelReserve is the channel_reserve_satoshis the sender requires of
	// the recipient. It is a policy parameter.
	ChannelReserve fn.Option[btcutil.Amount]

	// CsvDelay is the to_self_delay the sender requires of the recipient. It
	// is an epoch-affecting (script-rendering) parameter.
	CsvDelay fn.Option[uint16]

	// MaxAcceptedHtlcs is the sender's max_accepted_htlcs limit. It is a
	// policy parameter.
	MaxAcceptedHtlcs fn.Option[uint16]

	// ChannelFlags is the channel-wide announcement intent. It is neither an
	// epoch nor a per-side policy parameter; it applies to the whole
	// channel.
	ChannelFlags fn.Option[lnwire.FundingFlag]
}

// IsEmpty returns true if the proposal changes no parameter.
func (p ChannelParams) IsEmpty() bool {
	return p.DustLimit.IsNone() && p.MaxValueInFlight.IsNone() &&
		p.HtlcMinimum.IsNone() && p.ChannelReserve.IsNone() &&
		p.CsvDelay.IsNone() && p.MaxAcceptedHtlcs.IsNone() &&
		p.ChannelFlags.IsNone()
}

// AffectsEpoch returns true if the proposal changes a script/output-rendering
// parameter (CSV or dust) and therefore requires a commit-chain epoch entry.
func (p ChannelParams) AffectsEpoch() bool {
	return p.DustLimit.IsSome() || p.CsvDelay.IsSome()
}

// ChannelParamsFromDynPropose converts a wire lnwire.DynPropose message into
// the canonical ChannelParams representation.
func ChannelParamsFromDynPropose(dp *lnwire.DynPropose) ChannelParams {
	amt := func(b tlv.BigSizeT[btcutil.Amount]) btcutil.Amount {
		return b.Int()
	}

	return ChannelParams{
		DustLimit:        fn.MapOption(amt)(dp.DustLimit.ValOpt()),
		MaxValueInFlight: dp.MaxValueInFlight.ValOpt(),
		HtlcMinimum:      dp.HtlcMinimum.ValOpt(),
		ChannelReserve:   fn.MapOption(amt)(dp.ChannelReserve.ValOpt()),
		CsvDelay:         dp.CsvDelay.ValOpt(),
		MaxAcceptedHtlcs: dp.MaxAcceptedHTLCs.ValOpt(),
		ChannelFlags:     dp.ChannelFlags.ValOpt(),
	}
}

// ToDynPropose converts the canonical ChannelParams into a wire
// lnwire.DynPropose message for the given channel. Only the set fields are
// included in the message.
func (p ChannelParams) ToDynPropose(
	chanID lnwire.ChannelID) *lnwire.DynPropose {

	dp := &lnwire.DynPropose{ChanID: chanID}

	p.DustLimit.WhenSome(func(a btcutil.Amount) {
		var rec tlv.RecordT[tlv.TlvType0, tlv.BigSizeT[btcutil.Amount]]
		rec.Val = tlv.NewBigSizeT(a)
		dp.DustLimit = tlv.SomeRecordT(rec)
	})
	p.MaxValueInFlight.WhenSome(func(m lnwire.MilliSatoshi) {
		var rec tlv.RecordT[tlv.TlvType1, lnwire.MilliSatoshi]
		rec.Val = m
		dp.MaxValueInFlight = tlv.SomeRecordT(rec)
	})
	p.HtlcMinimum.WhenSome(func(m lnwire.MilliSatoshi) {
		var rec tlv.RecordT[tlv.TlvType2, lnwire.MilliSatoshi]
		rec.Val = m
		dp.HtlcMinimum = tlv.SomeRecordT(rec)
	})
	p.ChannelReserve.WhenSome(func(a btcutil.Amount) {
		var rec tlv.RecordT[tlv.TlvType3, tlv.BigSizeT[btcutil.Amount]]
		rec.Val = tlv.NewBigSizeT(a)
		dp.ChannelReserve = tlv.SomeRecordT(rec)
	})
	p.CsvDelay.WhenSome(func(c uint16) {
		var rec tlv.RecordT[tlv.TlvType4, uint16]
		rec.Val = c
		dp.CsvDelay = tlv.SomeRecordT(rec)
	})
	p.MaxAcceptedHtlcs.WhenSome(func(n uint16) {
		var rec tlv.RecordT[tlv.TlvType5, uint16]
		rec.Val = n
		dp.MaxAcceptedHTLCs = tlv.SomeRecordT(rec)
	})
	p.ChannelFlags.WhenSome(func(f lnwire.FundingFlag) {
		var rec tlv.RecordT[tlv.TlvType6, lnwire.FundingFlag]
		rec.Val = f
		dp.ChannelFlags = tlv.SomeRecordT(rec)
	})

	return dp
}

// ChannelParamsFromConfigs extracts the full set of dynamic parameters the
// given party currently advertises from the channel's local and remote configs
// plus the channel-wide flags. It is the inverse of ApplyToConfigs for a full
// param set, and is useful for reporting or seeding a proposal with the current
// values.
//
// The party's own dust limit governs its own commitment outputs and so is read
// from its own config. The remaining parameters are requirements the party
// imposes on its counterparty, and are therefore read from the counterparty's
// config (matching how open_channel/accept_channel are mapped to configs).
func ChannelParamsFromConfigs(proposer lntypes.ChannelParty, local,
	remote channeldb.ChannelConfig,
	flags lnwire.FundingFlag) ChannelParams {

	cfgs := lntypes.Dual[channeldb.ChannelConfig]{
		Local:  local,
		Remote: remote,
	}
	own := cfgs.GetForParty(proposer)
	counter := cfgs.GetForParty(proposer.CounterParty())

	return ChannelParams{
		DustLimit:        fn.Some(own.DustLimit),
		MaxValueInFlight: fn.Some(counter.MaxPendingAmount),
		HtlcMinimum:      fn.Some(counter.MinHTLC),
		ChannelReserve:   fn.Some(counter.ChanReserve),
		CsvDelay:         fn.Some(counter.CsvDelay),
		MaxAcceptedHtlcs: fn.Some(counter.MaxAcceptedHtlcs),
		ChannelFlags:     fn.Some(flags),
	}
}

// ApplyToConfigs applies the proposal on top of the given local and remote
// channel configs and returns the resulting configs. The inputs are not
// mutated.
//
// The proposer's own dust limit is written to the proposer's own config, while
// the remaining (non-flags) parameters are requirements the proposer imposes on
// its counterparty and are therefore written to the counterparty's config. This
// keeps the resulting configs consistent with how LND stores parameters from
// open_channel/accept_channel, and with the commit-chain epoch normalization.
//
// ChannelFlags is channel-wide and is not carried in a ChannelConfig, so it is
// not reflected here; callers apply it separately.
func (p ChannelParams) ApplyToConfigs(proposer lntypes.ChannelParty, local,
	remote channeldb.ChannelConfig) (channeldb.ChannelConfig,
	channeldb.ChannelConfig) {

	cfgs := lntypes.Dual[channeldb.ChannelConfig]{
		Local:  local,
		Remote: remote,
	}

	// The proposer's own dust limit lives in the proposer's own config.
	proposerCfg := cfgs.GetForParty(proposer)
	p.DustLimit.WhenSome(func(a btcutil.Amount) {
		proposerCfg.DustLimit = a
	})
	cfgs.SetForParty(proposer, proposerCfg)

	// The remaining parameters are requirements imposed on the counterparty
	// and so live in the counterparty's config.
	counterCfg := cfgs.GetForParty(proposer.CounterParty())
	p.CsvDelay.WhenSome(func(c uint16) {
		counterCfg.CsvDelay = c
	})
	p.ChannelReserve.WhenSome(func(a btcutil.Amount) {
		counterCfg.ChanReserve = a
	})
	p.MaxValueInFlight.WhenSome(func(m lnwire.MilliSatoshi) {
		counterCfg.MaxPendingAmount = m
	})
	p.HtlcMinimum.WhenSome(func(m lnwire.MilliSatoshi) {
		counterCfg.MinHTLC = m
	})
	p.MaxAcceptedHtlcs.WhenSome(func(n uint16) {
		counterCfg.MaxAcceptedHtlcs = n
	})
	cfgs.SetForParty(proposer.CounterParty(), counterCfg)

	return cfgs.Local, cfgs.Remote
}

// ValidateChannelParams checks that the resulting local and remote channel
// configs (i.e. the configs after a proposal has been applied) satisfy BOLT #2
// and LND policy. It validates the whole resulting channel, as mandated by the
// dynamic-commitments extension, rather than only the changed fields. All
// errors wrap one of the package sentinels so callers can match them with
// errors.Is.
func ValidateChannelParams(local, remote channeldb.ChannelConfig,
	capacity btcutil.Amount, maxLocalCSVDelay uint16) error {

	// The BOLT-mandated minimum dust limit (354 sat) coincides with LND's
	// minimum for an unknown-witness output; we reuse the maximum allowed
	// there as our upper bound too, matching open_channel validation.
	minDust := DustLimitForSize(input.UnknownWitnessSize)
	maxDust := 3 * minDust
	maxReserve := capacity / 5
	maxAccepted := uint16(input.MaxHTLCNumber / 2)

	// Per-side sanity checks. Each config holds the parameters that
	// constrain that party.
	sides := []struct {
		name string
		cfg  channeldb.ChannelConfig
	}{
		{"local", local},
		{"remote", remote},
	}
	for _, s := range sides {
		cfg := s.cfg

		switch {
		case cfg.DustLimit < minDust:
			return fmt.Errorf("%s dust limit %v: %w", s.name,
				cfg.DustLimit, ErrDynDustLimitTooSmall)

		case cfg.DustLimit > maxDust:
			return fmt.Errorf("%s dust limit %v (max %v): %w",
				s.name, cfg.DustLimit, maxDust,
				ErrDynDustLimitTooLarge)
		}

		if cfg.CsvDelay > maxLocalCSVDelay {
			return fmt.Errorf("%s csv delay %v (max %v): %w",
				s.name, cfg.CsvDelay, maxLocalCSVDelay,
				ErrDynCsvDelayTooLarge)
		}

		if cfg.ChanReserve > maxReserve {
			return fmt.Errorf("%s reserve %v (max %v): %w", s.name,
				cfg.ChanReserve, maxReserve,
				ErrDynReserveTooLarge)
		}

		if cfg.MaxAcceptedHtlcs > maxAccepted {
			return fmt.Errorf("%s max accepted htlcs %v (max %v): "+
				"%w", s.name, cfg.MaxAcceptedHtlcs, maxAccepted,
				ErrDynMaxAcceptedTooLarge)
		}

		if cfg.MinHTLC > cfg.MaxPendingAmount {
			return fmt.Errorf("%s htlc minimum %v exceeds max in "+
				"flight %v: %w", s.name, cfg.MinHTLC,
				cfg.MaxPendingAmount, ErrDynHtlcMinTooLarge)
		}
	}

	// Cross-peer check: the smaller of the two reserves must be at least the
	// larger of the two dust limits, so neither party can be pushed below
	// its own dust limit while still being above its reserve.
	minReserve := local.ChanReserve
	if remote.ChanReserve < minReserve {
		minReserve = remote.ChanReserve
	}
	maxDustLimit := local.DustLimit
	if remote.DustLimit > maxDustLimit {
		maxDustLimit = remote.DustLimit
	}
	if minReserve < maxDustLimit {
		return fmt.Errorf("min reserve %v below max dust limit %v: %w",
			minReserve, maxDustLimit, ErrDynReserveBelowDust)
	}

	return nil
}

// Validate computes the resulting channel configs for a proposal by the given
// party and validates them against BOLT #2 and LND policy. It is a convenience
// wrapper that combines ApplyToConfigs and ValidateChannelParams, and also
// rejects an empty proposal.
func (p ChannelParams) Validate(proposer lntypes.ChannelParty, local,
	remote channeldb.ChannelConfig, capacity btcutil.Amount,
	maxLocalCSVDelay uint16) error {

	if p.IsEmpty() {
		return ErrDynParamsNoChange
	}

	newLocal, newRemote := p.ApplyToConfigs(proposer, local, remote)

	return ValidateChannelParams(
		newLocal, newRemote, capacity, maxLocalCSVDelay,
	)
}

// ApplyChannelParams validates and atomically applies a dynamic-commitments
// parameter change proposed by the given party, persisting the result to disk
// and updating the in-memory channel state.
//
// The change is applied with proposer-owned semantics: it updates the values
// the proposer advertised at channel open (its own dust limit, and the
// requirements it imposes on its counterparty), plus the channel-wide
// ChannelFlags.
//
// When the proposal changes a script/output-rendering parameter (CSV or dust) a
// new commit-chain epoch is recorded at the given per-side lock-in heights, so
// historical commitment and HTLC scripts remain reconstructable. Policy-only
// proposals leave the epoch history untouched. lockInHeights gives the last
// height the outgoing parameters applied to on each commitment chain; it is
// only consulted for epoch-affecting changes.
//
// This is an API-level primitive: it performs the safe mutation and validation.
// It does not drive quiescence, negotiation, or the commitment dance; callers
// are responsible for invoking it at the point each chain locks in.
func (lc *LightningChannel) ApplyChannelParams(proposer lntypes.ChannelParty,
	params ChannelParams, maxLocalCSVDelay uint16,
	lockInHeights lntypes.Dual[uint64]) error {

	lc.Lock()
	defer lc.Unlock()

	if params.IsEmpty() {
		return ErrDynParamsNoChange
	}

	local := lc.channelState.LocalChanCfg
	remote := lc.channelState.RemoteChanCfg

	// Compute the resulting configs and validate the whole resulting set.
	newLocal, newRemote := params.ApplyToConfigs(proposer, local, remote)
	err := ValidateChannelParams(
		newLocal, newRemote, lc.channelState.Capacity, maxLocalCSVDelay,
	)
	if err != nil {
		return err
	}

	// Only script/output-rendering parameters (CSV/dust) produce an epoch
	// entry. Build one that carries the proposer's new dust limit and the
	// counterparty's new CSV, each defaulting to the current value so an
	// unchanged field is preserved.
	var epochChange fn.Option[channeldb.EpochParamChange]
	if params.AffectsEpoch() {
		cfgs := lntypes.Dual[channeldb.ChannelConfig]{
			Local:  local,
			Remote: remote,
		}
		proposerDust := params.DustLimit.UnwrapOr(
			cfgs.GetForParty(proposer).DustLimit,
		)
		counterCsv := params.CsvDelay.UnwrapOr(
			cfgs.GetForParty(proposer.CounterParty()).CsvDelay,
		)

		epochChange = fn.Some(channeldb.EpochParamChange{
			WhoSpecified: proposer,
			Params: channeldb.CommitmentParams{
				DustLimit: proposerDust,
				CsvDelay:  counterCsv,
			},
			LockInHeights: lockInHeights,
		})
	}

	newFlags := params.ChannelFlags.UnwrapOr(lc.channelState.ChannelFlags)

	return lc.channelState.ApplyChannelParams(
		newLocal.ChannelStateBounds, newRemote.ChannelStateBounds,
		newFlags, epochChange,
	)
}
