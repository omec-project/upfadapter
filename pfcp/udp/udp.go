// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package udp

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/omec-project/upfadapter/config"
	"github.com/omec-project/upfadapter/logger"
	"github.com/omec-project/upfadapter/types"
	"github.com/wmnsk/go-pfcp/message"
)

const (
	PFCP_PORT        = 8805
	PFCP_MAX_UDP_LEN = 2048
)

type ConsumerTable struct {
	m sync.Map // map[string]TxTable
}

type PfcpEventData struct {
	ErrHandler func(message.Message, error)
	LSEID      uint64
}

type PfcpServer struct {
	Addr *net.UDPAddr
	Conn *net.UDPConn
	// Consumer Table
	// Map Consumer IP to its tx table
	ConsumerTable ConsumerTable
}

var Server *PfcpServer

var (
	ServerStartTime time.Time
	CPNodeID        *types.NodeID
)

func (t *ConsumerTable) Load(consumerAddr string) (*TxTable, bool) {
	txTable, ok := t.m.Load(consumerAddr)
	if ok {
		return txTable.(*TxTable), ok
	}
	return nil, false
}

func (t *ConsumerTable) Store(consumerAddr string, txTable *TxTable) {
	t.m.Store(consumerAddr, txTable)
}

func init() {
	CPNodeID = &types.NodeID{NodeIdType: uint8(0), NodeIdValue: []byte(config.UpfAdapterIp)}
}

func PutTransaction(tx *Transaction) error {
	consumerAddr := tx.ConsumerAddr
	if _, exist := Server.ConsumerTable.Load(consumerAddr); !exist {
		Server.ConsumerTable.Store(consumerAddr, &TxTable{})
	}
	txTable, _ := Server.ConsumerTable.Load(consumerAddr)
	if _, exist := txTable.Load(tx.SequenceNumber); !exist {
		txTable.Store(tx.SequenceNumber, tx)
	} else {
		return fmt.Errorf("insert tx error: duplicate sequence number %d", tx.SequenceNumber)
	}
	return nil
}

func startTxLifeCycle(tx *Transaction) {
	sendErr := tx.Start()

	err := removeTransaction(tx)
	if err != nil {
		logger.PfcpLog.Warnln(err)
	}

	if sendErr != nil && tx.EventData != nil {
		if eventData, ok := tx.EventData.(PfcpEventData); ok {
			if errHandler := eventData.ErrHandler; errHandler != nil {
				msg, err := message.Parse(tx.SendMsg)
				if err != nil {
					logger.PfcpLog.Warnf("parse message error: %v", err)
					return
				}
				errHandler(msg, sendErr)
			}
		}
	}
}

func SendPfcp(msg message.Message, addr *net.UDPAddr, eventData interface{}) error {
	if Server == nil {
		return fmt.Errorf("PFCP server is not initialized")
	}
	if Server.Conn == nil {
		return fmt.Errorf("PFCP server is not listening")
	}

	buf := make([]byte, msg.MarshalLen())
	err := msg.MarshalTo(buf)
	if err != nil {
		return err
	}

	tx := NewTransaction(msg, buf, Server.Conn, addr, eventData)
	err = PutTransaction(tx)
	if err != nil {
		logger.PfcpLog.Errorf("failed to send PFCP message: %v", err)
		return err
	}
	go startTxLifeCycle(tx)
	return nil
}

func readPfcpMessage() (message.Message, error) {
	if Server == nil {
		return nil, fmt.Errorf("PFCP server is not initialized")
	}
	if Server.Conn == nil {
		return nil, fmt.Errorf("PFCP server is not listening")
	}

	buf := make([]byte, PFCP_MAX_UDP_LEN)
	n, addr, err := Server.Conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}

	msg, err := message.Parse(buf[:n])
	if err != nil {
		logger.PfcpLog.Errorf("error parsing PFCP message: %v", err)
		return nil, err
	}

	if IsRequest(msg) {
		// Todo: Implement SendingResponse type of reliable delivery
		tx, err := findTransaction(msg, addr)
		if err != nil {
			return msg, err
		} else if tx != nil {
			// err == nil && tx != nil => Resend Request
			err = fmt.Errorf("receive resend PFCP request")
			tx.EventChannel <- ReceiveResendRequest
			return msg, err
		} else {
			// err == nil && tx == nil => New Request
			return msg, nil
		}
	} else if IsResponse(msg) {
		tx, err := findTransaction(msg, Server.Addr)
		if err != nil {
			return msg, err
		}
		tx.EventChannel <- ReceiveValidResponse
	}

	return msg, nil
}

func findTransaction(msg message.Message, addr *net.UDPAddr) (*Transaction, error) {
	var tx *Transaction
	consumerAddr := addr.String()

	if Server == nil {
		return nil, fmt.Errorf("PFCP server is not initialized")
	}

	if IsResponse(msg) {
		if _, exist := Server.ConsumerTable.Load(consumerAddr); !exist {
			return nil, fmt.Errorf("txTable not found")
		}

		txTable, _ := Server.ConsumerTable.Load(consumerAddr)
		seqNum := msg.Sequence()

		if _, exist := txTable.Load(seqNum); !exist {
			return nil, fmt.Errorf("sequence number [%d] not found", seqNum)
		}

		tx, _ = txTable.Load(seqNum)
	} else if IsRequest(msg) {
		if _, exist := Server.ConsumerTable.Load(consumerAddr); !exist {
			return nil, nil
		}
		txTable, _ := Server.ConsumerTable.Load(consumerAddr)
		seqNum := msg.Sequence()
		if _, exist := txTable.Load(seqNum); !exist {
			return nil, nil
		}
		tx, _ = txTable.Load(seqNum)
	}
	return tx, nil
}

func Run(Dispatch func(message.Message)) {
	addr := &net.UDPAddr{
		IP:   net.ParseIP(CPNodeID.ResolveNodeIdToIp().String()),
		Port: PFCP_PORT,
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		logger.PfcpLog.Errorf("failed to listen on %s: %v", addr.String(), err)
		return
	}
	Server = &PfcpServer{
		Addr: addr,
		Conn: conn,
	}
	logger.PfcpLog.Infof("listen on %s", addr.String())

	go func() {
		for {
			pfcpMessage, err := readPfcpMessage()
			if err != nil {
				if err.Error() == "Receive resend PFCP request" {
					logger.PfcpLog.Infoln(err)
				} else {
					logger.PfcpLog.Warnf("read PFCP error: %v", err)
				}
				continue
			}
			go Dispatch(pfcpMessage)
		}
	}()

	ServerStartTime = time.Now()
}

func removeTransaction(tx *Transaction) error {
	if Server == nil {
		return fmt.Errorf("PFCP server is not initialized")
	}
	consumerAddr := tx.ConsumerAddr
	txTable, _ := Server.ConsumerTable.Load(consumerAddr)

	if txTmp, exist := txTable.Load(tx.SequenceNumber); exist {
		tx = txTmp
		if tx.TxType == SendingRequest {
			logger.PfcpLog.Debugf("remove request transaction [%d]", tx.SequenceNumber)
		} else if tx.TxType == SendingResponse {
			logger.PfcpLog.Debugf("remove response transaction [%d]", tx.SequenceNumber)
		}

		txTable.Delete(tx.SequenceNumber)
	} else {
		return fmt.Errorf("remove tx error: transaction [%d] doesn't exist", tx.SequenceNumber)
	}
	return nil
}
