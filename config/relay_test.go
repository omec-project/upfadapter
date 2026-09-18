// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omec-project/upfadapter/types"
)

// withSmfAddr isolates the package-level SMF address so ordering between tests cannot
// decide their outcome.
func withSmfAddr(t *testing.T, value net.IP) {
	t.Helper()

	smfAddrMutex.Lock()
	previous := smfAddr
	smfAddr = value
	smfAddrMutex.Unlock()

	t.Cleanup(func() {
		smfAddrMutex.Lock()
		smfAddr = previous
		smfAddrMutex.Unlock()
	})
}

func TestSmfAddrUnknownUntilAnSmfSpeaks(t *testing.T) {
	withSmfAddr(t, nil)

	if addr := SmfAddr(); addr != nil {
		t.Fatalf("SmfAddr() = %v, want nil before any SMF message", addr)
	}
}

func TestSetSmfAddrRecordsWhereToRelay(t *testing.T) {
	withSmfAddr(t, nil)

	SetSmfAddr("10.42.0.188")

	addr := SmfAddr()
	if addr == nil {
		t.Fatal("SmfAddr() = nil, want the recorded address")
	}

	if !addr.IP.Equal(net.ParseIP("10.42.0.188")) || addr.Port != PfcpPort {
		t.Errorf("SmfAddr() = %v, want 10.42.0.188:%d", addr, PfcpPort)
	}
}

// An SMF-initiated message without the field must not erase an address we already have,
// or one malformed request would stop every later report from being relayed.
func TestSetSmfAddrIgnoresEmpty(t *testing.T) {
	withSmfAddr(t, net.ParseIP("10.42.0.188"))

	SetSmfAddr("")

	if addr := SmfAddr(); addr == nil || !addr.IP.Equal(net.ParseIP("10.42.0.188")) {
		t.Errorf("SmfAddr() = %v, want the previously recorded address", addr)
	}
}

// The address is claimed in an unauthenticated request body. A claim that is not an IP
// must be refused where it arrives: stored unparsed it erased an address that worked, and
// every report was then rejected until the next SMF message happened to carry a good one.
func TestSetSmfAddrRefusesAClaimThatIsNotAnIP(t *testing.T) {
	withSmfAddr(t, net.ParseIP("10.42.0.188"))

	SetSmfAddr("upf-adapter.local")

	if addr := SmfAddr(); addr == nil || !addr.IP.Equal(net.ParseIP("10.42.0.188")) {
		t.Errorf("SmfAddr() = %v, want the previously recorded address", addr)
	}

	withSmfAddr(t, nil)

	SetSmfAddr("upf-adapter.local")

	if addr := SmfAddr(); addr != nil {
		t.Errorf("SmfAddr() = %v, want nil for a claim that is not an IP", addr)
	}
}

// The recorded address is handed out while the package keeps writing the field, so what
// a caller gets must not be the stored slice itself.
func TestSmfAddrReturnsACopy(t *testing.T) {
	withSmfAddr(t, net.ParseIP("10.42.0.188"))

	addr := SmfAddr()
	addr.IP[len(addr.IP)-1] = 0

	if again := SmfAddr(); again == nil || !again.IP.Equal(net.ParseIP("10.42.0.188")) {
		t.Errorf("SmfAddr() = %v after a caller wrote to its address, want 10.42.0.188", again)
	}
}

// withReportRelays isolates the relay table and the sequence counter, so a test that
// leaves an entry behind -- or stops at a Fatalf before taking one -- cannot decide
// another test's outcome.
func withReportRelays(t *testing.T) {
	t.Helper()

	reportRelayMutex.Lock()
	relays, byHost, seq, swept := reportRelays, relaysByHost, reportRelaySeq, lastRelaySweep
	reportRelays, relaysByHost, reportRelaySeq = make(map[uint32]reportRelay), make(map[string]map[uint32]struct{}), 0
	// The sweep clock too: left at another test's wall-clock reading, a case that records its
	// entries in the past would find the throttle closed and never sweep at all.
	lastRelaySweep = time.Time{}
	reportRelayMutex.Unlock()

	t.Cleanup(func() {
		reportRelayMutex.Lock()
		reportRelays, relaysByHost, reportRelaySeq, lastRelaySweep = relays, byHost, seq, swept
		reportRelayMutex.Unlock()
	})
}

// relaySequence is RelayReportSequence for the tests that have no reason to exhaust the range:
// the allocator only refuses when every number in it is outstanding, and none of these hold more
// than a handful.
func relaySequence(t *testing.T, upfAddr *net.UDPAddr, upfSeq uint32, now time.Time) (uint32, bool) {
	t.Helper()

	relaySeq, fresh, err := RelayReportSequence(upfAddr, upfSeq, now, RelayGeneration(upfAddr.IP.String()))
	if err != nil {
		t.Fatalf("RelayReportSequence(%v, seq[%d]): %v", upfAddr, upfSeq, err)
	}

	return relaySeq, fresh
}

// A relayed report is renumbered into the adapter's own space, and the answer must carry
// the number the user plane is waiting for -- not the adapter's.
func TestRelayReportSequenceKeepsTheUpfsOwnNumber(t *testing.T) {
	withReportRelays(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}

	relaySeq, _ := relaySequence(t, upfAddr, 4711, time.Now())

	if relaySeq == 4711 {
		t.Error("the report was relayed under the UPF's own sequence number, which shares a table with the SMF's")
	}

	if relaySeq < relaySequenceFloor || relaySeq > relaySequenceCeil {
		t.Errorf("relay sequence %#x outside the adapter's range [%#x, %#x]",
			relaySeq, relaySequenceFloor, relaySequenceCeil)
	}

	addr, upfSeq := TakeReportRelay(relaySeq)
	if addr == nil || !addr.IP.Equal(upfAddr.IP) || addr.Port != upfAddr.Port {
		t.Fatalf("TakeReportRelay(%d) address = %v, want %v", relaySeq, addr, upfAddr)
	}

	if upfSeq != 4711 {
		t.Errorf("TakeReportRelay(%d) UPF sequence = %d, want 4711", relaySeq, upfSeq)
	}

	// Taken exactly once, so a duplicate response cannot be forwarded to a stale peer.
	if again, _ := TakeReportRelay(relaySeq); again != nil {
		t.Errorf("TakeReportRelay(%d) second call = %v, want nil", relaySeq, again)
	}
}

// Two user planes number their reports independently, so the same number arriving from
// both must still resolve to the right peer.
func TestRelayReportSequenceSeparatesTwoUpfsUsingTheSameNumber(t *testing.T) {
	withReportRelays(t)

	first := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}
	second := &net.UDPAddr{IP: net.ParseIP("10.42.0.185"), Port: PfcpPort}

	now := time.Now()

	firstRelay, firstFresh := relaySequence(t, first, 1, now)
	secondRelay, secondFresh := relaySequence(t, second, 1, now)

	// Both are reports in their own right. Recognising a retransmission by its sequence
	// number alone would make the second one the first one arriving twice.
	if !firstFresh || !secondFresh {
		t.Fatalf("reports from two user planes were taken for one: fresh = %v and %v, want both true", firstFresh, secondFresh)
	}

	if firstRelay == secondRelay {
		t.Fatalf("both reports were relayed as seq[%d]; one origin has been lost", firstRelay)
	}

	firstAddr, firstSeq := TakeReportRelay(firstRelay)
	secondAddr, secondSeq := TakeReportRelay(secondRelay)

	if firstAddr == nil || !firstAddr.IP.Equal(first.IP) {
		t.Errorf("first report resolved to %v, want %v", firstAddr, first)
	}

	if secondAddr == nil || !secondAddr.IP.Equal(second.IP) {
		t.Errorf("second report resolved to %v, want %v", secondAddr, second)
	}

	if firstSeq != 1 || secondSeq != 1 {
		t.Errorf("restored UPF sequences = %d and %d, want 1 and 1", firstSeq, secondSeq)
	}
}

// A user-plane function retransmits a request it has not been answered. Until the answer
// exists there is no response transaction holding the report, so nothing else recognises
// the copy: relaying it as a report of its own raises a second downlink data notification
// for traffic the SMF is already being told about.
func TestRelayReportSequenceRecognisesARetransmission(t *testing.T) {
	withReportRelays(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.187"), Port: PfcpPort}

	now := time.Now()

	first, fresh := relaySequence(t, upfAddr, 7, now)
	if !fresh {
		t.Fatal("the first report was taken for a retransmission")
	}

	again, fresh := relaySequence(t, upfAddr, 7, now.Add(3*time.Second))
	if fresh {
		t.Error("a retransmission was relayed as a report of its own")
	}

	if again != first {
		t.Errorf("retransmission resolved to seq[%d], want the outstanding seq[%d]", again, first)
	}

	// The next report is numbered as though the retransmission had never arrived, which is
	// what says no second entry was made for it.
	other, fresh := relaySequence(t, upfAddr, 8, now)
	if !fresh || other != first+1 {
		t.Errorf("the next report = seq[%d] (fresh = %v), want seq[%d]: the retransmission consumed a sequence number",
			other, fresh, first+1)
	}

	TakeReportRelay(other)
	ForgetReportRelay(other)

	if addr, _ := TakeReportRelay(first); addr == nil {
		t.Fatalf("TakeReportRelay(%d) = nil, want the outstanding relay", first)
	}

	ForgetReportRelay(first)

	// Once the answer has been sent and the exchange forgotten, the same number is a new
	// report again -- the user plane's counter comes round, and this must not make it
	// unreachable. Taking alone does not end it: see
	// TestTakeReportRelayKeepsTheEntryUntilItIsForgotten for what the entry is still doing
	// between the two.
	repeated, fresh := relaySequence(t, upfAddr, 7, now)
	if !fresh {
		t.Error("a report reusing the number of an exchange already answered was refused as a retransmission")
	}

	TakeReportRelay(repeated)
	ForgetReportRelay(repeated)
}

// A peer is an address and a port -- that is the identity every other transaction in the
// adapter is keyed by -- so two user planes behind one address are two peers, and a report
// from each is a report of its own.
func TestRelayReportSequenceSeparatesTwoPortsAtOneAddress(t *testing.T) {
	withReportRelays(t)

	first := &net.UDPAddr{IP: net.ParseIP("10.42.0.189"), Port: PfcpPort}
	second := &net.UDPAddr{IP: net.ParseIP("10.42.0.189"), Port: PfcpPort + 1}

	now := time.Now()

	firstRelay, firstFresh := relaySequence(t, first, 1, now)

	secondRelay, secondFresh := relaySequence(t, second, 1, now)
	if !firstFresh || !secondFresh {
		t.Fatalf("reports from two ports were taken for one: fresh = %v and %v, want both true", firstFresh, secondFresh)
	}

	if firstRelay == secondRelay {
		t.Fatalf("both reports were relayed as seq[%d]; one origin has been lost", firstRelay)
	}

	addr, _ := TakeReportRelay(secondRelay)
	if addr == nil || addr.Port != second.Port {
		t.Errorf("second report resolved to %v, want %v", addr, second)
	}

	TakeReportRelay(firstRelay)
}

// An entry past its lifetime is forgotten, not read as a relay still in flight: the user
// plane would otherwise never have that report relayed at all.
func TestRelayReportSequenceRelaysAgainOnceTheOutstandingEntryExpired(t *testing.T) {
	withReportRelays(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.188"), Port: PfcpPort}

	now := time.Now()

	stale, _ := relaySequence(t, upfAddr, 11, now.Add(-2*reportRelayLifetime))

	relaySeq, fresh := relaySequence(t, upfAddr, 11, now)
	if !fresh {
		t.Error("a report whose outstanding entry had expired was taken for a retransmission")
	}

	if relaySeq == stale {
		t.Errorf("the expired entry was handed back as seq[%d]", stale)
	}

	TakeReportRelay(relaySeq)
}

// Sequence numbers are three octets on the wire, so the counter has to come back round
// inside the adapter's own range rather than overflow into the SMF's.
func TestRelayReportSequenceWrapsWithinItsOwnRange(t *testing.T) {
	withReportRelays(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}

	reportRelayMutex.Lock()
	reportRelaySeq = relaySequenceCeil
	reportRelayMutex.Unlock()

	relaySeq, _ := relaySequence(t, upfAddr, 7, time.Now())
	if relaySeq != relaySequenceFloor {
		t.Errorf("sequence after the ceiling = %#x, want %#x", relaySeq, relaySequenceFloor)
	}

	TakeReportRelay(relaySeq)
}

// An SMF that never answers must not cost an entry for the life of the process.
func TestReportRelayForgetsEntriesTheSmfNeverAnswered(t *testing.T) {
	withReportRelays(t)

	stale := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}
	fresh := &net.UDPAddr{IP: net.ParseIP("10.42.0.185"), Port: PfcpPort}

	now := time.Now()

	staleRelay, _ := relaySequence(t, stale, 1, now.Add(-2*reportRelayLifetime))
	freshRelay, _ := relaySequence(t, fresh, 2, now)

	if addr, _ := TakeReportRelay(staleRelay); addr != nil {
		t.Errorf("TakeReportRelay(%d) = %v, want nil for an entry past its lifetime", staleRelay, addr)
	}

	if addr, _ := TakeReportRelay(freshRelay); addr == nil {
		t.Errorf("TakeReportRelay(%d) = nil, want the entry recorded just now", freshRelay)
	}
}

func TestTakeReportRelayUnknownSequence(t *testing.T) {
	withReportRelays(t)

	if addr, _ := TakeReportRelay(999999); addr != nil {
		t.Errorf("TakeReportRelay(unknown) = %v, want nil", addr)
	}
}

// isolateUpfAddrs empties the recorded user planes so ordering between tests cannot
// decide their outcome. Both directions of the index are replaced: leaving the reverse one
// behind would let a node id recorded by an earlier test decide where this one thinks it
// moved from.
func isolateUpfAddrs(t *testing.T) {
	t.Helper()

	upfAddrMutex.Lock()
	previousAddrs, previousNodes := upfAddrs, upfNodeAddrs
	upfAddrs = make(map[string]map[string]struct{})
	upfNodeAddrs = make(map[string]upfNodeRecord)
	upfAddrMutex.Unlock()

	t.Cleanup(func() {
		upfAddrMutex.Lock()
		upfAddrs, upfNodeAddrs = previousAddrs, previousNodes
		upfAddrMutex.Unlock()
	})
}

// Before the SMF has named any user plane there is nothing to relay for, and a report
// from an unknown source must not be taken on trust.
func TestNoUpfIsKnownBeforeTheSmfNamesOne(t *testing.T) {
	isolateUpfAddrs(t)

	if IsKnownUpfAddr(net.ParseIP("10.42.0.184")) {
		t.Error("IsKnownUpfAddr() = true with nothing recorded, want false")
	}
}

func TestRecordUpfAddrByIP(t *testing.T) {
	isolateUpfAddrs(t)

	RecordUpfAddr(types.NewNodeID("10.42.0.184"))

	if !IsKnownUpfAddr(net.ParseIP("10.42.0.184")) {
		t.Error("IsKnownUpfAddr() = false for the user plane the SMF named, want true")
	}

	// The peer address read off the socket is a 4-byte IPv4 where the node ID resolved to
	// a 16-byte one; the two must still be the same user plane.
	if !IsKnownUpfAddr(net.ParseIP("10.42.0.184").To4()) {
		t.Error("IsKnownUpfAddr() = false for the same address in 4-byte form, want true")
	}
}

// Anything else that can reach the port is refused, which is the whole point: N4 has no
// transport authentication, so the SMF naming a user plane is the only thing that makes
// it one.
func TestIsKnownUpfAddrRefusesAnySourceTheSmfDidNotName(t *testing.T) {
	isolateUpfAddrs(t)

	RecordUpfAddr(types.NewNodeID("10.42.0.184"))

	if IsKnownUpfAddr(net.ParseIP("10.42.0.185")) {
		t.Error("IsKnownUpfAddr() = true for a source the SMF never named, want false")
	}

	if IsKnownUpfAddr(nil) {
		t.Error("IsKnownUpfAddr(nil) = true, want false")
	}
}

// A user plane configured by name is matched by the address it actually sends from, not
// by the name, because the peer address is all a report carries.
func TestRecordUpfAddrResolvesAnFqdn(t *testing.T) {
	isolateUpfAddrs(t)

	types.InsertDnsHostIp("upf.5gc.svc", net.ParseIP("10.42.0.190"))

	RecordUpfAddr(types.NewNodeID("upf.5gc.svc"))

	if !IsKnownUpfAddr(net.ParseIP("10.42.0.190")) {
		t.Error("IsKnownUpfAddr() = false for the address an FQDN-configured user plane resolves to, want true")
	}
}

// An unresolvable name yields IPv4zero, and recording that would admit an address no
// real peer has. The value is driven in directly rather than through a name that does
// not resolve, so the test does not depend on a DNS lookup failing.
func TestRecordUpfAddrIgnoresAnUnspecifiedAddress(t *testing.T) {
	isolateUpfAddrs(t)

	RecordUpfAddr(&types.NodeID{NodeIdType: types.NodeIdTypeIpv4Address, NodeIdValue: net.IPv4zero.To4()})

	if IsKnownUpfAddr(net.IPv4zero) {
		t.Error("IsKnownUpfAddr(0.0.0.0) = true, want false")
	}
}

// 0.0.0.0 and :: are IP addresses and not destinations. A claim carrying one used to be
// stored like any other, and every report relayed afterwards went to an address that
// answers nothing -- until some later SMF message happened to carry a usable one.
func TestSetSmfAddrRefusesTheUnspecifiedAddress(t *testing.T) {
	working := net.ParseIP("10.10.0.5")

	for _, claim := range []string{"0.0.0.0", "::"} {
		withSmfAddr(t, working)

		SetSmfAddr(claim)

		addr := SmfAddr()
		if addr == nil || !addr.IP.Equal(working) {
			t.Errorf("after a claimed SMF address of %q, SmfAddr() = %v, want the working address %v",
				claim, addr, working)
		}
	}
}

// A user plane reached by name can move: the pod comes back at another address, and the
// name now resolves there. The address it left must stop authorising reports, or an
// unrelated peer that takes that address over is relayed for as though the SMF had named
// it -- and the set grows for the life of the process.
func TestRecordUpfAddrForgetsTheAddressANodeLeft(t *testing.T) {
	isolateUpfAddrs(t)

	types.InsertDnsHostIp("moving.5gc.svc", net.ParseIP("10.42.0.200"))
	RecordUpfAddr(types.NewNodeID("moving.5gc.svc"))

	types.InsertDnsHostIp("moving.5gc.svc", net.ParseIP("10.42.0.201"))
	RecordUpfAddr(types.NewNodeID("moving.5gc.svc"))

	if IsKnownUpfAddr(net.ParseIP("10.42.0.200")) {
		t.Error("IsKnownUpfAddr() = true for the address the user plane left, want false")
	}

	if !IsKnownUpfAddr(net.ParseIP("10.42.0.201")) {
		t.Error("IsKnownUpfAddr() = false for the address the user plane moved to, want true")
	}
}

// One address can carry two identities -- the SMF may name the same user plane by FQDN in
// one message and by IP in another -- so an address is forgotten only once nothing is left
// at it. Forgetting on the first move would revoke a peer that is still there.
func TestRecordUpfAddrKeepsAnAddressAnotherIdentityStillUses(t *testing.T) {
	isolateUpfAddrs(t)

	shared := net.ParseIP("10.42.0.210")

	types.InsertDnsHostIp("shared.5gc.svc", shared)
	RecordUpfAddr(types.NewNodeID("shared.5gc.svc"))
	RecordUpfAddr(types.NewNodeID("10.42.0.210"))

	types.InsertDnsHostIp("shared.5gc.svc", net.ParseIP("10.42.0.211"))
	RecordUpfAddr(types.NewNodeID("shared.5gc.svc"))

	if !IsKnownUpfAddr(shared) {
		t.Error("IsKnownUpfAddr() = false for an address the SMF still names by IP, want true")
	}
}

// Answering a report does not end the adapter's need to recognise copies of it. The
// response transaction that absorbs them exists only once the answer has been sent, and
// messages are handled on their own goroutines, so an entry deleted at the moment of
// taking leaves a window in which a retransmission is relayed a second time -- a second
// downlink data notification, and a second page.
func TestTakeReportRelayKeepsTheEntryUntilItIsForgotten(t *testing.T) {
	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.220"), Port: PfcpPort}
	now := time.Now()

	relaySeq, fresh := relaySequence(t, upfAddr, 7, now)
	if !fresh {
		t.Fatalf("precondition: the first relay of a report must be fresh")
	}

	if addr, _ := TakeReportRelay(relaySeq); addr == nil {
		t.Fatalf("TakeReportRelay(%d) = nil, want the origin of the report", relaySeq)
	}

	if _, fresh := relaySequence(t, upfAddr, 7, now); fresh {
		t.Error("a retransmission arriving while the answer is being sent was treated as a new report; " +
			"it would be relayed again and raise a second notification")
	}

	ForgetReportRelay(relaySeq)

	if _, fresh := relaySequence(t, upfAddr, 7, now); !fresh {
		t.Error("a report arriving after the exchange was forgotten was not treated as new")
	}
}

// The SMF's answer and the give-up path both end the same exchange. Only one of them may
// answer the user plane, so the second taker is told there is nothing to take.
func TestTakeReportRelayIsTakeOnce(t *testing.T) {
	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.221"), Port: PfcpPort}

	relaySeq, _ := relaySequence(t, upfAddr, 9, time.Now())
	t.Cleanup(func() { ForgetReportRelay(relaySeq) })

	if addr, _ := TakeReportRelay(relaySeq); addr == nil {
		t.Fatalf("precondition: the first take must return the origin")
	}

	if addr, _ := TakeReportRelay(relaySeq); addr != nil {
		t.Errorf("TakeReportRelay(%d) = %v on the second take, want nil: both paths would answer the same report",
			relaySeq, addr)
	}
}

// A user plane the SMF stops naming altogether ages out, and its address with it. Nothing else
// removes it: the set is learned from traffic, so without this an address stays authorised for
// the life of the process and a later, unrelated occupant of it passes the source gate.
func TestAnAddressIsForgottenOnceTheSmfStopsNamingIt(t *testing.T) {
	isolateUpfAddrs(t)

	RecordUpfAddr(types.NewNodeID("10.42.0.230"))

	if !IsKnownUpfAddr(net.ParseIP("10.42.0.230")) {
		t.Fatalf("precondition: the user plane the SMF named must be known")
	}

	// Age the record rather than the clock: the sweep runs off time.Now() inside RecordUpfAddr.
	upfAddrMutex.Lock()
	for node, record := range upfNodeAddrs {
		record.seen = record.seen.Add(-upfAddrLifetime - time.Minute)
		upfNodeAddrs[node] = record
	}
	upfAddrMutex.Unlock()

	// Any later message sweeps.
	RecordUpfAddr(types.NewNodeID("10.42.0.231"))

	if IsKnownUpfAddr(net.ParseIP("10.42.0.230")) {
		t.Error("an address the SMF has not named for longer than its lifetime is still authorised")
	}

	if !IsKnownUpfAddr(net.ParseIP("10.42.0.231")) {
		t.Error("the user plane named just now was swept away with the stale one")
	}
}

// A name that fails to resolve is not a user plane that has gone. The SMF is still addressing it,
// so the identity stays named and its address stays authorised -- revoking on a DNS blip would
// stop relaying the reports this exists to carry.
func TestANameThatStopsResolvingKeepsTheAddressItHad(t *testing.T) {
	isolateUpfAddrs(t)

	types.InsertDnsHostIp("blinking.5gc.svc", net.ParseIP("10.42.0.240"))
	RecordUpfAddr(types.NewNodeID("blinking.5gc.svc"))

	// Age it, so the next record must refresh the clock for the address to survive the sweep.
	upfAddrMutex.Lock()
	for node, record := range upfNodeAddrs {
		record.seen = record.seen.Add(-upfAddrLifetime - time.Minute)
		upfNodeAddrs[node] = record
	}
	upfAddrMutex.Unlock()

	// An FQDN that does not resolve yields the zero address, which is what this models.
	types.InsertDnsHostIp("blinking.5gc.svc", net.IPv4zero)
	RecordUpfAddr(types.NewNodeID("blinking.5gc.svc"))

	RecordUpfAddr(types.NewNodeID("10.42.0.241"))

	if !IsKnownUpfAddr(net.ParseIP("10.42.0.240")) {
		t.Error("the address was revoked because the name stopped resolving, while the SMF was still naming it")
	}
}

// The allocator must not hand out a number an exchange is still waiting on. When the counter comes
// round, replacing a live entry would return the SMF's answer to the wrong user plane and leave
// the first report unanswered.
func TestRelayReportSequenceSkipsANumberStillOutstanding(t *testing.T) {
	withReportRelays(t)

	first := &net.UDPAddr{IP: net.ParseIP("10.42.0.250"), Port: PfcpPort}
	second := &net.UDPAddr{IP: net.ParseIP("10.42.0.251"), Port: PfcpPort}
	now := time.Now()

	held, fresh, err := RelayReportSequence(first, 1, now, RelayGeneration(first.IP.String()))
	if err != nil || !fresh {
		t.Fatalf("precondition: the first report must be relayed (fresh=%v, err=%v)", fresh, err)
	}

	// Wind the counter back so the next candidate is the number just handed out.
	reportRelayMutex.Lock()
	reportRelaySeq = held - 1
	reportRelayMutex.Unlock()

	next, fresh, err := RelayReportSequence(second, 2, now, RelayGeneration(second.IP.String()))
	if err != nil || !fresh {
		t.Fatalf("the second report was not relayed (fresh=%v, err=%v)", fresh, err)
	}

	if next == held {
		t.Errorf("seq[%d] was handed out twice; the first exchange would never be answered", held)
	}

	if addr, _ := TakeReportRelay(held); addr == nil || !addr.IP.Equal(first.IP) {
		t.Errorf("the outstanding entry under seq[%d] = %v, want the user plane that is waiting on it", held, addr)
	}
}

// The lifetime has to hold when reports are the only traffic. The sweep runs on SMF messages, so
// an SMF that has gone quiet is both the case the lifetime exists for and the case in which
// nothing would ever sweep -- and the gate is asked on user-plane traffic, which keeps arriving.
func TestAStaleAddressIsRefusedEvenWithNoSmfTrafficToSweepIt(t *testing.T) {
	isolateUpfAddrs(t)

	RecordUpfAddr(types.NewNodeID("10.42.0.26"))

	upfAddrMutex.Lock()
	for node, record := range upfNodeAddrs {
		record.seen = record.seen.Add(-upfAddrLifetime - time.Minute)
		upfNodeAddrs[node] = record
	}
	upfAddrMutex.Unlock()

	// No RecordUpfAddr in between: nothing has swept, and nothing will until the SMF speaks.
	if IsKnownUpfAddr(net.ParseIP("10.42.0.26")) {
		t.Error("a report was accepted for an address the SMF has not named for longer than its lifetime")
	}
}

// The assignable range includes its ceiling: the counter resets only once it is past it. That is
// what makes the allocator's candidate count one more than the difference -- trying one fewer
// would report the range exhausted while the number the counter sits on is free.
func TestTheRelaySequenceRangeIncludesItsCeiling(t *testing.T) {
	withReportRelays(t)

	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.27"), Port: PfcpPort}

	reportRelayMutex.Lock()
	reportRelaySeq = relaySequenceCeil - 1
	reportRelayMutex.Unlock()

	if got, _ := relaySequence(t, upfAddr, 1, time.Now()); got != relaySequenceCeil {
		t.Errorf("the number after the last but one = %d, want the ceiling %d", got, relaySequenceCeil)
	}

	if got, _ := relaySequence(t, upfAddr, 2, time.Now()); got != relaySequenceFloor {
		t.Errorf("the number after the ceiling = %d, want the floor %d", got, relaySequenceFloor)
	}
}

// The number the search starts from is the one the wrap reaches last, so a trip count one short
// of the range skips exactly it -- and that is the number still free when every other is taken.
// Tested over a range small enough to fill; the live one holds 8 388 608 numbers.
func TestNextFreeRelaySequenceFindsTheOneTheWrapReachesLast(t *testing.T) {
	const floor, ceil uint32 = 10, 13

	outstanding := map[uint32]reportRelay{10: {}, 11: {}, 13: {}}

	got, free := nextFreeRelaySequence(outstanding, 12, floor, ceil)
	if !free {
		t.Fatal("the range was reported exhausted while 12 was free")
	}

	if got != 12 {
		t.Errorf("free number = %d, want 12", got)
	}
}

// And it does report exhaustion when the range really is full, rather than handing out a number
// an exchange is waiting on.
func TestNextFreeRelaySequenceReportsAFullRange(t *testing.T) {
	const floor, ceil uint32 = 10, 13

	outstanding := map[uint32]reportRelay{10: {}, 11: {}, 12: {}, 13: {}}

	if got, free := nextFreeRelaySequence(outstanding, 12, floor, ceil); free {
		t.Errorf("free number = %d, want none: every number in the range is outstanding", got)
	}
}

// The release happens inside the lock that de-authorises the address, not after it. Releasing
// afterwards leaves room for the SMF to name the address again and for a report from it to be
// answered in between, and the release would then throw away what that answer is holding.
func TestAnAddressThatIsLeftIsReleasedUnderTheLock(t *testing.T) {
	isolateUpfAddrs(t)

	var released []string

	previous := upfAddrReleased
	OnUpfAddrReleased(func(addr string) {
		// Held while the hook runs: taking it here would deadlock if the release were not
		// inside the critical section, which is the property this pins.
		if upfAddrMutex.TryLock() {
			upfAddrMutex.Unlock()
			t.Error("the address was released after the lock was dropped")
		}

		released = append(released, addr)
	})

	t.Cleanup(func() { OnUpfAddrReleased(previous) })

	types.InsertDnsHostIp("leaving.5gc.svc", net.ParseIP("10.42.0.28"))
	RecordUpfAddr(types.NewNodeID("leaving.5gc.svc"))

	types.InsertDnsHostIp("leaving.5gc.svc", net.ParseIP("10.42.0.29"))
	RecordUpfAddr(types.NewNodeID("leaving.5gc.svc"))

	if len(released) != 1 || released[0] != "10.42.0.28" {
		t.Errorf("released %v, want the address the user plane left", released)
	}
}

// The same for an address that ages out rather than being left.
func TestAnAddressThatAgesOutIsReleased(t *testing.T) {
	isolateUpfAddrs(t)

	var released []string

	previous := upfAddrReleased
	OnUpfAddrReleased(func(addr string) { released = append(released, addr) })

	t.Cleanup(func() { OnUpfAddrReleased(previous) })

	RecordUpfAddr(types.NewNodeID("10.42.0.30"))

	upfAddrMutex.Lock()
	for node, record := range upfNodeAddrs {
		record.seen = record.seen.Add(-upfAddrLifetime - time.Minute)
		upfNodeAddrs[node] = record
	}
	upfAddrMutex.Unlock()

	RecordUpfAddr(types.NewNodeID("10.42.0.31"))

	if len(released) != 1 || released[0] != "10.42.0.30" {
		t.Errorf("released %v, want the address that aged out", released)
	}
}

// A relay sequence number the SMF is already using is refused, and the report has to go out under
// another one. The claim on the report must survive that: it is what a retransmission arriving
// meanwhile is recognised by, and dropping it to allocate again left the report unclaimed in
// between -- so that copy was relayed on its own account, and one report from one user plane
// became two downlink data notifications.
func TestRenumberingAReportKeepsItClaimed(t *testing.T) {
	withReportRelays(t)

	upf := &net.UDPAddr{IP: net.ParseIP("10.42.0.190"), Port: PfcpPort}
	now := time.Now()

	first, fresh := relaySequence(t, upf, 9, now)
	if !fresh {
		t.Fatal("the first report was not treated as fresh")
	}

	second, err := RenumberReportRelay(first)
	if err != nil {
		t.Fatalf("renumbering the relay: %v", err)
	}

	if second == first {
		t.Errorf("RenumberReportRelay(%d) = %d, want a different number", first, second)
	}

	// The retransmission the renumber has to survive.
	held, stillFresh := relaySequence(t, upf, 9, now)
	if stillFresh {
		t.Error("a retransmission arriving during the renumber was taken for a fresh report; the SMF gets a second notification for one report")
	}

	if held != second {
		t.Errorf("the retransmission is held as %d, want the renumbered %d", held, second)
	}

	if addr, upfSeq := TakeReportRelay(first); addr != nil {
		t.Errorf("TakeReportRelay(%d) = %v/%d, want nil: nothing is outstanding under the number that was refused", first, addr, upfSeq)
	}

	if addr, _ := TakeReportRelay(second); addr == nil {
		t.Errorf("TakeReportRelay(%d) = nil, want the peer the report came from", second)
	}
}

// An address stops being a user plane the SMF names, and its outstanding relays go with it.
//
// Addresses are reused: a pod that leaves gives its address to the next one. The new occupant's
// first report can carry a sequence number the previous occupant had outstanding, and with the
// old origin still held it is matched as that report's retransmission -- so the new peer's report
// is never relayed, and the answer to the old one is delivered to a user plane that never sent it.
func TestARelayIsForgottenWithTheUserPlaneThatRaisedIt(t *testing.T) {
	isolateUpfAddrs(t)
	withReportRelays(t)

	left := net.ParseIP("10.42.0.220")
	upf := &net.UDPAddr{IP: left, Port: PfcpPort}

	types.InsertDnsHostIp("leaving.5gc.svc", left)
	RecordUpfAddr(types.NewNodeID("leaving.5gc.svc"))

	outstanding, _ := relaySequence(t, upf, 4, time.Now())

	// The node moves, which is what releases the address it was at.
	types.InsertDnsHostIp("leaving.5gc.svc", net.ParseIP("10.42.0.221"))
	RecordUpfAddr(types.NewNodeID("leaving.5gc.svc"))

	if addr, _ := TakeReportRelay(outstanding); addr != nil {
		t.Errorf("TakeReportRelay(%d) = %v, want nil: the user plane it belonged to is gone", outstanding, addr)
	}

	// The address's next occupant, numbering its reports from its own counter.
	successor, fresh := relaySequence(t, upf, 4, time.Now())
	if !fresh {
		t.Error("a new user plane's report was taken for the previous occupant's retransmission, so it is never relayed")
	}

	if successor == outstanding {
		t.Errorf("the successor was given the number the previous occupant held (%d)", successor)
	}

	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	if _, held := relaysByHost[left.String()][outstanding]; held {
		t.Error("the index still names the forgotten relay, so it outlives the entry it points at")
	}
}

// The index and the table it indexes are written together, so nothing keyed by a peer outlives
// the entries it names -- a leak here would be invisible until the map held every peer the
// adapter had ever relayed for.
func TestTheHostIndexDoesNotOutliveItsEntries(t *testing.T) {
	withReportRelays(t)

	upf := &net.UDPAddr{IP: net.ParseIP("10.42.0.230"), Port: PfcpPort}

	relaySeq, _ := relaySequence(t, upf, 11, time.Now())
	ForgetReportRelay(relaySeq)

	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	if held := len(relaysByHost); held != 0 {
		t.Errorf("relaysByHost holds %d hosts after the only entry was forgotten, want none", held)
	}
}

// The sweep is throttled so it does not run on every packet, but it still has to run: an SMF that
// stops answering must not cost an entry per report for the life of the process.
func TestTheSweepStillReclaimsWhatTheSmfNeverAnswered(t *testing.T) {
	withReportRelays(t)

	now := time.Now()

	for i := range 4 {
		relaySequence(t, &net.UDPAddr{IP: net.ParseIP("10.42.0.240"), Port: PfcpPort + i}, uint32(i), now.Add(-2*reportRelayLifetime))
	}

	// One later report, far enough on that the throttle is open.
	relaySequence(t, &net.UDPAddr{IP: net.ParseIP("10.42.0.241"), Port: PfcpPort}, 99, now)

	reportRelayMutex.Lock()
	defer reportRelayMutex.Unlock()

	if held := len(reportRelays); held != 1 {
		t.Errorf("reportRelays holds %d entries, want only the one inside its lifetime", held)
	}
}

// The window the renumber closes is a race, so this is what actually holds it shut: while a report
// is being moved from one number to the next, a retransmission of it must never be seen as fresh.
//
// It was two operations before -- forget the entry, then claim a number again -- with the lock
// released in between, and a copy of the report arriving in that gap found nothing claimed and was
// relayed on its own account. Renumbering under one hold is what removes the gap; this fails if it
// is ever reopened.
func TestNoRetransmissionSlipsThroughARenumber(t *testing.T) {
	withReportRelays(t)

	if runtime.GOMAXPROCS(0) < 2 {
		// One processor serialises the two goroutines and the window never opens, so the test
		// would pass on a machine where the defect is still present.
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))
	}

	upf := &net.UDPAddr{IP: net.ParseIP("10.42.0.250"), Port: PfcpPort}

	relaySeq, fresh := relaySequence(t, upf, 12, time.Now())
	if !fresh {
		t.Fatal("the report was not claimed to begin with")
	}

	var (
		wg      sync.WaitGroup
		slipped atomic.Bool
	)

	done := make(chan struct{})

	wg.Add(1)

	go func() {
		defer wg.Done()
		defer close(done)

		held := relaySeq

		for range 2000 {
			next, err := RenumberReportRelay(held)
			if err != nil {
				return
			}

			held = next
		}
	}()

	wg.Add(1)

	go func() {
		defer wg.Done()

		for {
			select {
			case <-done:
				return
			default:
			}

			if _, fresh, err := RelayReportSequence(upf, 12, time.Now(), RelayGeneration(upf.IP.String())); err == nil && fresh {
				slipped.Store(true)

				return
			}
		}
	}()

	wg.Wait()

	if slipped.Load() {
		t.Error("a retransmission was taken for a fresh report while its relay was being renumbered; the SMF is told twice about one report, and pages twice")
	}
}

// Two messages naming the same user plane can be in flight at once, and the address is resolved
// before the lock is taken -- so the one that resolved first can reach the lock second. Applied in
// that order it would authorise the address the node has moved away from and forget the one it
// moved to, which is the state this bookkeeping exists to keep straight.
func TestAnOlderResolutionDoesNotUndoANewerOne(t *testing.T) {
	isolateUpfAddrs(t)

	moved := net.ParseIP("10.42.0.61")

	// The newer message: the node is now at .61.
	types.InsertDnsHostIp("moving.5gc.svc", moved)
	RecordUpfAddr(types.NewNodeID("moving.5gc.svc"))

	// The older one, resolved before the move and arriving after it.
	stale := upfNodeRecord{addr: "10.42.0.60", seen: time.Now().Add(-time.Minute)}
	applyStaleResolution(t, "moving.5gc.svc", stale)

	if IsKnownUpfAddr(net.ParseIP("10.42.0.60")) {
		t.Error("the address the user plane left was authorised again by a resolution that predates the move")
	}

	if !IsKnownUpfAddr(moved) {
		t.Error("the address the user plane moved to was forgotten by a resolution that predates the move")
	}
}

// applyStaleResolution delivers a message whose resolution is older than what the node's record
// already holds, which is what a slow resolver produces when two messages overlap.
func applyStaleResolution(t *testing.T, fqdn string, older upfNodeRecord) {
	t.Helper()

	types.InsertDnsHostIp(fqdn, net.ParseIP(older.addr))

	upfAddrMutex.Lock()
	record := upfNodeAddrs[nodeKey(types.NewNodeID(fqdn))]
	record.seen = time.Now().Add(time.Minute)
	upfNodeAddrs[nodeKey(types.NewNodeID(fqdn))] = record
	upfAddrMutex.Unlock()

	RecordUpfAddr(types.NewNodeID(fqdn))
}

// A rollback drops its own registration and only its own.
//
// Sequence numbers are the SMF's, and a second request can carry one that is already in flight
// here -- a retransmission does so by definition. Deleting by number alone takes that request's
// registration instead, and the fault this rollback exists to repair then happens to it: its
// answer arrives, finds nothing waiting for it, and is dropped, while it waits for one that was
// given away.
func TestARollbackLeavesAnotherRequestsRegistrationAlone(t *testing.T) {
	const seq = uint32(7788)

	withUpfTxns(t)

	failed := make(PfcpTxnChan, 1)
	InsertUpfPfcpTxn(seq, failed)

	// The second request, registering under the same number before the first rolls back.
	current := make(PfcpTxnChan, 1)
	InsertUpfPfcpTxn(seq, current)

	ForgetUpfPfcpTxn(seq, failed)

	held := GetUpfPfcpTxn(seq)
	if held == nil {
		t.Fatal("the rollback took the registration of a request that is still waiting; its answer would be dropped and it would wait for one given away")
	}

	if held != current {
		t.Error("the registration under this number is not the one that is waiting")
	}
}

// withUpfTxns isolates the transaction table, which is process-wide.
func withUpfTxns(t *testing.T) {
	t.Helper()

	UpfTxnsMutex.Lock()
	previous := UpfTxns
	UpfTxns = make(map[uint32]PfcpTxnChan)
	UpfTxnsMutex.Unlock()

	t.Cleanup(func() {
		UpfTxnsMutex.Lock()
		UpfTxns = previous
		UpfTxnsMutex.Unlock()
	})
}

// The source check and the claim are two operations, and the address can be released between them.
// Claiming anyway leaves a claim for a peer the adapter no longer relays for -- and an address in a
// cluster is reused, so the next occupant's report matches it as a retransmission and is answered
// with this one's response.
func TestAClaimIsRefusedWhenThePeerWasReleasedAfterTheCheck(t *testing.T) {
	isolateUpfAddrs(t)
	withReportRelays(t)

	peer := net.ParseIP("10.42.0.70")
	upf := &net.UDPAddr{IP: peer, Port: PfcpPort}

	types.InsertDnsHostIp("leaving.5gc.svc", peer)
	RecordUpfAddr(types.NewNodeID("leaving.5gc.svc"))

	// What the handler reads before it tests the source.
	authorisedUnder := RelayGeneration(peer.String())

	// And the release that lands between that test and the claim.
	types.InsertDnsHostIp("leaving.5gc.svc", net.ParseIP("10.42.0.71"))
	RecordUpfAddr(types.NewNodeID("leaving.5gc.svc"))

	_, _, err := RelayReportSequence(upf, 5, time.Now(), authorisedUnder)
	if !errors.Is(err, ErrPeerReleased) {
		t.Errorf("RelayReportSequence() error = %v, want %v: the claim outlives the authorisation that allowed it", err, ErrPeerReleased)
	}
}

// And a claim made under the generation in force is not refused, so the check above costs an
// ordinary report nothing.
func TestAClaimUnderTheCurrentGenerationIsAllowed(t *testing.T) {
	isolateUpfAddrs(t)
	withReportRelays(t)

	peer := net.ParseIP("10.42.0.72")
	upf := &net.UDPAddr{IP: peer, Port: PfcpPort}

	RecordUpfAddr(types.NewNodeID("10.42.0.72"))

	if _, fresh, err := RelayReportSequence(upf, 6, time.Now(), RelayGeneration(peer.String())); err != nil || !fresh {
		t.Errorf("RelayReportSequence() = fresh %v, err %v; want a fresh claim", fresh, err)
	}
}
