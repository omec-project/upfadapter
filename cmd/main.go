// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"github.com/omec-project/upfadapter/internal/logger"
	"github.com/omec-project/upfadapter/internal/service"
)

var UPFAdapter = &service.UPFAdapter{}

func main() {
	UPFAdapter.Initialize()
	if err := UPFAdapter.Start(); err != nil {
		logger.AppLog.Fatalf("UPF adapter run error: %v", err)
	}
}
