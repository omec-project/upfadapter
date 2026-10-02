// SPDX-FileCopyrightText: 2026 Open Networking Foundation
//
// SPDX-License-Identifier: Apache-2.0

package udp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wmnsk/go-pfcp/message"
)

// A closed socket fails every read at once, so a loop that only logged the error
// and went on would spin for as long as the process lives.
func TestReadLoopReturnsWhenSocketIsClosed(t *testing.T) {
	conn, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	udpConn := conn.(*net.UDPConn)

	previous := Server
	Server = &PfcpServer{Addr: udpConn.LocalAddr().(*net.UDPAddr), Conn: udpConn}
	t.Cleanup(func() { Server = previous })

	done := make(chan struct{})
	go func() {
		defer close(done)
		readLoop(func(message.Message, *net.UDPAddr) {})
	}()

	if err := udpConn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readLoop did not return after the socket was closed")
	}
}

// serve is what Run blocks in: cancelling the context must close the socket and
// return only after the read loop has stopped, so the process does not exit
// with the socket still open.
func TestServeClosesSocketOnCancel(t *testing.T) {
	conn, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	udpConn := conn.(*net.UDPConn)

	previous := Server
	Server = &PfcpServer{Addr: udpConn.LocalAddr().(*net.UDPAddr), Conn: udpConn}
	t.Cleanup(func() { Server = previous })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(ctx, udpConn, func(message.Message, *net.UDPAddr) {})
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serve did not return after the context was cancelled")
	}
	if _, err := udpConn.WriteToUDP([]byte{0}, udpConn.LocalAddr().(*net.UDPAddr)); err == nil {
		t.Fatal("the socket is still open after serve returned")
	}
}
