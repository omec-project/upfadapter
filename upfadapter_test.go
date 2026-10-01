// SPDX-FileCopyrightText: 2026 Open Networking Foundation
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeShutsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, server, time.Second) }()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned %v after a cancel, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return after the context was cancelled")
	}
}

func TestServeReturnsListenError(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer busy.Close()

	server := &http.Server{Addr: busy.Addr().String(), ReadHeaderTimeout: time.Second}
	if err := serve(context.Background(), server, time.Second); err == nil {
		t.Fatal("serve returned nil for an address already in use")
	}
}
