//go:build integration

package itest

import (
	"fmt"

	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lntest"
)

// excludedTestsWindows is a list of tests that are flaky on Windows and should
// be excluded from the test suite atm.
//
// TODO(yy): fix these tests and remove them from this list.
var excludedTestsWindows = []string{
	"batch channel funding",
	"zero conf channel open",
	"open channel with unstable utxos",
	"funding flow persistence",
	"channel policy update public zero conf",

	"listsweeps",
	"sweep htlcs",
	"sweep cpfp anchor incoming timeout",
	"payment succeeded htlc remote swept",
	"3rd party anchor spend",

	"send payment amp",
	"async payments benchmark",
	"async bidirectional payments",

	"multihop htlc aggregation leased",
	"multihop htlc aggregation leased zero conf",
	"multihop htlc aggregation anchor",
	"multihop htlc aggregation anchor zero conf",
	"multihop htlc aggregation simple taproot",
	"multihop htlc aggregation simple taproot zero conf",

	"channel force closure anchor",
	"channel force closure simple taproot",
	"channel backup restore force close",
	"wipe forwarding packages",

	"coop close with htlcs",
	"coop close with external delivery",

	"forward interceptor restart",
	"forward interceptor dedup htlcs",
	"invoice HTLC modifier basic",
	"lookup htlc resolution",

	"remote signer taproot",
	"remote signer account import",
	"remote signer bump fee",
	"remote signer funding input types",
	"remote signer funding async payments taproot",
	"remote signer funding async payments",
	"remote signer random seed",
	"remote signer verify msg",
	"remote signer channel open",
	"remote signer shared key",
	"remote signer psbt",
	"remote signer sign output raw",

	"on chain to blinded",
	"query blinded route",

	"data loss protection",
}

// filterWindowsFlakyTests filters out the flaky tests that are excluded from
// the test suite on Windows.
func filterWindowsFlakyTests() []*lntest.TestCase {
	filteredTestCases := make([]*lntest.TestCase, 0, len(allTestCases))

	excludedSet := fn.NewSet(excludedTestsWindows...)
	for _, tc := range allTestCases {
		if excludedSet.Contains(tc.Name) {
			excludedSet.Remove(tc.Name)

			continue
		}

		filteredTestCases = append(filteredTestCases, tc)
	}

	if excludedSet.IsEmpty() {
		return filteredTestCases
	}

	for _, name := range excludedSet.ToSlice() {
		fmt.Println("Test not found in test suite:", name)
	}

	panic("excluded tests not found in test suite")
}
