// SPDX-FileCopyrightText: 2026 Forsway Scandinavia AB
//
// SPDX-License-Identifier: Apache-2.0

package udp

import (
	"context"
	"net"
	"testing"

	"github.com/wmnsk/go-pfcp/ie"
	"github.com/wmnsk/go-pfcp/message"
)

// Answering a user plane leaves a transaction table keyed by its own address, and nothing in the
// transaction machinery removes an empty one -- so without this, every peer ever answered costs an
// entry for the life of the process. The release happens when the SMF stops naming that user
// plane, at which point its reports are refused anyway.
func TestDeleteByHostReleasesEveryPortSeenForOnePeer(t *testing.T) {
	table := &ConsumerTable{}

	for _, addr := range []string{"10.0.0.1:8805", "10.0.0.1:41234", "10.0.0.2:8805"} {
		table.LoadOrStore(addr, &TxTable{})
	}

	table.DeleteByHost("10.0.0.1")

	for _, gone := range []string{"10.0.0.1:8805", "10.0.0.1:41234"} {
		if _, found := table.Load(gone); found {
			t.Errorf("%s is still held; the peer chooses its source port, so one guessed port is not enough", gone)
		}
	}

	if _, found := table.Load("10.0.0.2:8805"); !found {
		t.Error("another user plane's table was released with it")
	}
}

// A transaction is removed by the goroutine that ran it, which finds its table by the peer's
// address — and that address can belong to a different table by then, since releasing a peer drops
// its table and a peer named again gets a new one. Removing by sequence number alone would take a
// live successor's response out of its resend window, and the next retransmission would be relayed
// to the SMF as a new report.
func TestRemovingATransactionLeavesItsSuccessorAlone(t *testing.T) {
	packet, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer packet.Close()

	conn, ok := packet.(*net.UDPConn)
	if !ok {
		t.Fatalf("listener is %T, want *net.UDPConn", packet)
	}

	previous := Server
	Server = &PfcpServer{Addr: conn.LocalAddr().(*net.UDPAddr), Conn: conn}

	t.Cleanup(func() { Server = previous })

	peer := &net.UDPAddr{IP: net.ParseIP("127.0.0.3"), Port: PFCP_PORT}

	response := func() *Transaction {
		msg := message.NewSessionReportResponse(0, 0, 0x1, 7, 0, ie.NewCause(ie.CauseRequestAccepted))
		buf := make([]byte, msg.MarshalLen())

		if err := msg.MarshalTo(buf); err != nil {
			t.Fatalf("marshal: %v", err)
		}

		return NewTransaction(msg, buf, conn, peer, nil)
	}

	stale := response()
	if err := PutTransaction(stale); err != nil {
		t.Fatalf("holding the first response: %v", err)
	}

	// The peer stops being one, and is then named again: a new table, a new response, the same
	// address and the same sequence number.
	ForgetConsumer("127.0.0.3")

	successor := response()
	if err := PutTransaction(successor); err != nil {
		t.Fatalf("holding the successor's response: %v", err)
	}

	if err := removeTransaction(stale); err == nil {
		t.Error("removing the released peer's transaction reported success against a table it no longer belongs to")
	}

	held, found := Server.ConsumerTable.LoadOrStore(peer.String(), &TxTable{}).Load(7)
	if !found || held != successor {
		t.Error("the successor's response was removed by the goroutine of the transaction that preceded it")
	}
}

// Registering a transaction is a lookup and an insertion, and a release landing between them
// detaches the table the insertion writes into. What is put there cannot be found again -- the
// next lookup for that peer makes a fresh table -- so the sender would be told its message went
// out while nothing could match the response or absorb a retransmission.
//
// This is the question PutTransaction asks afterwards to catch that, asserted on its own: the
// interleaving itself cannot be reproduced from a test, because the window is two adjacent
// operations wide inside the function, and under a releaser fast enough to hit it the result is
// indistinguishable from a release that landed one instant later.
func TestATableTakenAwayIsNoLongerThePeersTable(t *testing.T) {
	table := &ConsumerTable{}
	peer := "10.42.0.31:8805"

	held := table.LoadOrStore(peer, &TxTable{})

	if !table.stillHolds(peer, held) {
		t.Fatal("the table just stored is not the peer's table")
	}

	table.DeleteByHost("10.42.0.31")

	if table.stillHolds(peer, held) {
		t.Error("a table the release took away is still reported as the peer's, so a transaction registered into it would be reported as reachable")
	}

	replacement := table.LoadOrStore(peer, &TxTable{})

	if replacement == held {
		t.Fatal("the release did not detach the table")
	}

	if table.stillHolds(peer, held) {
		t.Error("the table that was replaced is still reported as the peer's; whatever was put in it is invisible to every lookup")
	}
}
