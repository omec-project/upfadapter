// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/omec-project/upfadapter/config"
	"github.com/omec-project/upfadapter/logger"
	"github.com/omec-project/upfadapter/pfcp"
	"github.com/omec-project/upfadapter/pfcp/udp"
	"github.com/wmnsk/go-pfcp/message"
	"go.uber.org/zap/zapcore"
)

// Handler for SMF initiated msgs
func handler(w http.ResponseWriter, req *http.Request) {
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		logger.AppLog.Errorf("server: could not read request body: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	var udpPodMsg config.UdpPodPfcpMsg
	err = json.Unmarshal(reqBody, &udpPodMsg)
	if err != nil {
		logger.AppLog.Errorln("error unmarshalling pfcp msg")
		return
	}

	pfcpMessage, err := message.Parse(udpPodMsg.Msg.Body)
	if err != nil {
		logger.AppLog.Errorln("error parsing pfcp msg")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	logger.AppLog.Debugf("received msg type [%v], upf nodeId [%s], smfIp [%v], msg [%v]",
		pfcpMessage.MessageType(), udpPodMsg.UpNodeID.NodeIdValue, udpPodMsg.SmfIp, udpPodMsg.Msg)

	// Remember where the SMF is. A message the user-plane function originates arrives
	// with no request of ours to answer, so this is the only address we can relay it to.
	//
	// The address is taken from the body rather than from the connection, and the mismatch
	// is reported rather than refused. The SMF fills it with its own first non-loopback
	// address, which is the peer address in an ordinary deployment -- but not necessarily
	// one where the SMF is multi-homed or reached through a proxy, and refusing there would
	// break the relay for a difference that is legitimate. A claim that does not match the
	// peer is worth seeing, so it is logged once per change.
	if host, _, splitErr := net.SplitHostPort(req.RemoteAddr); splitErr == nil &&
		udpPodMsg.SmfIp != "" && udpPodMsg.SmfIp != host {
		// Only when the claim changes. Every PFCP message the SMF sends arrives here, so a
		// persistent mismatch would otherwise warn on each one.
		if current := config.SmfAddr(); current == nil || current.IP.String() != udpPodMsg.SmfIp {
			logger.AppLog.Warnf("message claims SMF address [%s] but arrived from [%s]; relaying to the claimed address",
				udpPodMsg.SmfIp, host)
		}
	}

	config.SetSmfAddr(udpPodMsg.SmfIp)

	// Remember the user plane too. A session report arrives on the UDP socket with no
	// request of ours to match it against, so this is what tells a user plane the SMF
	// actually talks to apart from anything else that can reach the port.
	config.RecordUpfAddr(&udpPodMsg.UpNodeID)

	pfcpJsonRsp, err := pfcp.ForwardPfcpMsgToUpf(pfcpMessage, udpPodMsg.UpNodeID)
	if err != nil {
		logger.AppLog.Errorf("error forwarding pfcp msg to UPF: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(pfcpJsonRsp)
	if err != nil {
		logger.AppLog.Errorf("error writing pfcp msg: %v", err)
	}
	logger.AppLog.Debugf("response sent for %v", pfcpMessage.MessageType())
}

// Handler for msgs from SMF
func main() {
	if lvl := os.Getenv("UPFADAPTER_LOG_LEVEL"); lvl != "" {
		if level, err := zapcore.ParseLevel(lvl); err != nil {
			logger.CfgLog.Warnf("invalid UPFADAPTER_LOG_LEVEL [%s]; keeping current log level. Accepted values include: debug, info, warn, error, dpanic, panic, fatal.", lvl)
		} else {
			logger.SetLogLevel(level)
		}
	}

	// SIGTERM (a pod delete or a rollout) and Ctrl-C stop the HTTP server and
	// close the PFCP socket; main then returns and the process exits 0.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// UDP handler for pfcp msg from UPF
	pfcpDone := make(chan struct{})
	go func() {
		defer close(pfcpDone)
		udp.Run(ctx, pfcp.Dispatch)
	}()

	http.HandleFunc("/", handler)
	server := &http.Server{Addr: ":8090", ReadHeaderTimeout: 10 * time.Second}
	if err := serve(ctx, server, 5*time.Second); err != nil {
		logger.AppLog.Errorf("error listening TCP connection: %v", err)
		return
	}
	// Return only once the PFCP socket is closed.
	<-pfcpDone
	logger.AppLog.Infoln("UPF adapter terminated")
}

// serve runs server until ctx is cancelled, then shuts it down, waiting at
// most timeout for requests in flight. It returns an error only if the server
// fails before that.
func serve(ctx context.Context, server *http.Server, timeout time.Duration) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	logger.AppLog.Infoln("terminating UPF adapter")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.AppLog.Warnf("HTTP server shutdown: %v", err)
	}
	return nil
}
