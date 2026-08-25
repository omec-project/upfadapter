// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"net"
	"testing"
	"time"
)

// withSmfAddr isolates the package-level SMF address so ordering between tests cannot
// decide their outcome.
func withSmfAddr(t *testing.T, value string) {
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
	withSmfAddr(t, "")

	if addr := SmfAddr(); addr != nil {
		t.Fatalf("SmfAddr() = %v, want nil before any SMF message", addr)
	}
}

func TestSetSmfAddrRecordsWhereToRelay(t *testing.T) {
	withSmfAddr(t, "")

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
	withSmfAddr(t, "10.42.0.188")

	SetSmfAddr("")

	if addr := SmfAddr(); addr == nil || !addr.IP.Equal(net.ParseIP("10.42.0.188")) {
		t.Errorf("SmfAddr() = %v, want the previously recorded address", addr)
	}
}

func TestSmfAddrRejectsNonIP(t *testing.T) {
	withSmfAddr(t, "upf-adapter.local")

	if addr := SmfAddr(); addr != nil {
		t.Errorf("SmfAddr() = %v, want nil for a value that is not an IP", addr)
	}
}

// A relayed report is renumbered into the adapter's own space, and the answer must carry
// the number the user plane is waiting for -- not the adapter's.
func TestRelayReportSequenceKeepsTheUpfsOwnNumber(t *testing.T) {
	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}

	relaySeq := RelayReportSequence(upfAddr, 4711, time.Now())
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
	first := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}
	second := &net.UDPAddr{IP: net.ParseIP("10.42.0.185"), Port: PfcpPort}

	now := time.Now()

	firstRelay := RelayReportSequence(first, 1, now)
	secondRelay := RelayReportSequence(second, 1, now)

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

// Sequence numbers are three octets on the wire, so the counter has to come back round
// inside the adapter's own range rather than overflow into the SMF's.
func TestRelayReportSequenceWrapsWithinItsOwnRange(t *testing.T) {
	upfAddr := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}

	reportRelayMutex.Lock()
	previous := reportRelaySeq
	reportRelaySeq = relaySequenceCeil
	reportRelayMutex.Unlock()

	t.Cleanup(func() {
		reportRelayMutex.Lock()
		reportRelaySeq = previous
		reportRelayMutex.Unlock()
	})

	relaySeq := RelayReportSequence(upfAddr, 7, time.Now())
	if relaySeq != relaySequenceFloor {
		t.Errorf("sequence after the ceiling = %#x, want %#x", relaySeq, relaySequenceFloor)
	}

	TakeReportRelay(relaySeq)
}

// An SMF that never answers must not cost an entry for the life of the process.
func TestReportRelayForgetsEntriesTheSmfNeverAnswered(t *testing.T) {
	stale := &net.UDPAddr{IP: net.ParseIP("10.42.0.184"), Port: PfcpPort}
	fresh := &net.UDPAddr{IP: net.ParseIP("10.42.0.185"), Port: PfcpPort}

	now := time.Now()

	staleRelay := RelayReportSequence(stale, 1, now.Add(-2*reportRelayLifetime))
	freshRelay := RelayReportSequence(fresh, 2, now)

	if addr, _ := TakeReportRelay(staleRelay); addr != nil {
		t.Errorf("TakeReportRelay(%d) = %v, want nil for an entry past its lifetime", staleRelay, addr)
	}

	if addr, _ := TakeReportRelay(freshRelay); addr == nil {
		t.Errorf("TakeReportRelay(%d) = nil, want the entry recorded just now", freshRelay)
	}
}

func TestTakeReportRelayUnknownSequence(t *testing.T) {
	if addr, _ := TakeReportRelay(999999); addr != nil {
		t.Errorf("TakeReportRelay(unknown) = %v, want nil", addr)
	}
}
