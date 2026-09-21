// Copyright 2019 free5GC.org
// Copyright 2024 Canonical Ltd.
//
// SPDX-License-Identifier: Apache-2.0

package udp

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/omec-project/upfadapter/logger"
	"github.com/wmnsk/go-pfcp/message"
)

type TransactionType uint8

type TxTable struct {
	m sync.Map // map[uint32]*Transaction
}

// LoadOrStore inserts tx for sequenceNumber unless one is already there, and reports which
// happened. Load-then-Store is not the same thing: two goroutines numbering requests at once can
// both find a sequence free and both store, and the second silently replaces a transaction whose
// response is still to come.
func (t *TxTable) LoadOrStore(sequenceNumber uint32, tx *Transaction) (*Transaction, bool) {
	existing, loaded := t.m.LoadOrStore(sequenceNumber, tx)

	return existing.(*Transaction), loaded
}

func (t *TxTable) Load(sequenceNumber uint32) (*Transaction, bool) {
	if t == nil {
		logger.PfcpLog.Warnf("TxTable is nil")
		return nil, false
	}

	tx, ok := t.m.Load(sequenceNumber)
	if ok {
		return tx.(*Transaction), ok
	}
	return nil, false
}

// DeleteIf removes the transaction under sequenceNumber only while it is still this one.
//
// A transaction is removed by the goroutine that ran it, which loads the table by the peer's
// address. That address can belong to a different table by then: releasing a peer drops its
// table, and a peer named again gets a new one. Deleting by sequence number alone would then
// remove a live successor's transaction -- and a response's job is to stay until its resend
// window ends, so removing it early lets the next retransmission through as a new report.
func (t *TxTable) DeleteIf(sequenceNumber uint32, tx *Transaction) bool {
	if t == nil {
		return false
	}

	return t.m.CompareAndDelete(sequenceNumber, tx)
}

const (
	SendingRequest TransactionType = iota
	SendingResponse
)

const (
	NumOfResend                 = 3
	ResendRequestTimeOutPeriod  = 3
	ResendResponseTimeOutPeriod = 15
)

type Transaction struct {
	EventChannel   chan EventType
	Conn           *net.UDPConn
	DestAddr       *net.UDPAddr
	ConsumerAddr   string
	ErrHandler     func(*message.Message, error)
	EventData      interface{}
	SendMsg        []byte
	SequenceNumber uint32
	MessageType    uint8
	TxType         TransactionType
}

func NewTransaction(pfcpMSG message.Message, binaryMSG []byte, Conn *net.UDPConn, DestAddr *net.UDPAddr, eventData interface{}) *Transaction {
	tx := &Transaction{
		SendMsg:        binaryMSG,
		SequenceNumber: pfcpMSG.Sequence(),
		MessageType:    pfcpMSG.MessageType(),
		EventChannel:   make(chan EventType, 1),
		Conn:           Conn,
		DestAddr:       DestAddr,
		EventData:      eventData,
	}

	if IsRequest(pfcpMSG) {
		tx.TxType = SendingRequest
		tx.ConsumerAddr = Conn.LocalAddr().String()
	} else if IsResponse(pfcpMSG) {
		tx.TxType = SendingResponse
		tx.ConsumerAddr = DestAddr.String()
	}
	logger.PfcpLog.Debugf("new Transaction SEQ[%d] DestAddr[%s]", tx.SequenceNumber, DestAddr.String())
	return tx
}

func (transaction *Transaction) Start() error {
	logger.PfcpLog.Debugf("start Transaction [%d]", transaction.SequenceNumber)

	if transaction.TxType == SendingRequest {
		for iter := 0; iter < NumOfResend; iter++ {
			timer := time.NewTimer(ResendRequestTimeOutPeriod * time.Second)
			_, err := transaction.Conn.WriteToUDP(transaction.SendMsg, transaction.DestAddr)
			if err != nil {
				logger.PfcpLog.Warnf("request Transaction [%d]: %s", transaction.SequenceNumber, err)
				return err
			}

			select {
			case event := <-transaction.EventChannel:

				if event == ReceiveValidResponse {
					logger.PfcpLog.Debugf("request Transaction [%d]: receive valid response", transaction.SequenceNumber)
					return nil
				}
			case <-timer.C:
				logger.PfcpLog.Debugf("request Transaction [%d]: timeout expire", transaction.SequenceNumber)
				logger.PfcpLog.Debugf("request Transaction [%d]: Resend packet", transaction.SequenceNumber)
				continue
			}
		}
		// Num of retries exhausted, send failure back to app
		return fmt.Errorf("request timeout, seq [%d]", transaction.SequenceNumber)
	} else if transaction.TxType == SendingResponse {
		// Todo :Implement SendingResponse type of reliable delivery
		timer := time.NewTimer(ResendResponseTimeOutPeriod * time.Second)
		for iter := 0; iter < NumOfResend; iter++ {
			_, err := transaction.Conn.WriteToUDP(transaction.SendMsg, transaction.DestAddr)
			if err != nil {
				logger.PfcpLog.Warnf("response Transaction [%d]: sending error", transaction.SequenceNumber)
				return err
			}

			select {
			case event := <-transaction.EventChannel:

				if event == ReceiveResendRequest {
					logger.PfcpLog.Debugf("response Transaction [%d]: receive resend request", transaction.SequenceNumber)
					logger.PfcpLog.Debugf("response Transaction [%d]: Resend packet", transaction.SequenceNumber)

					// The deadline is per quiet period, not per transaction. Started once and never
					// reset, it measured from the first answer -- so a peer still retransmitting
					// when it expired outlived the answer that was absorbing those copies, and the
					// next one reached the handler as a new request. Each resend asks for the
					// window again.
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}

					timer.Reset(ResendResponseTimeOutPeriod * time.Second)

					continue
				}
			case <-timer.C:
				logger.PfcpLog.Debugf("response Transaction [%d]: timeout expire", transaction.SequenceNumber)
				return fmt.Errorf("response timeout, seq [%d]", transaction.SequenceNumber)
			}
		}
	}
	return nil
}
