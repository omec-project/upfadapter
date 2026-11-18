// SPDX-FileCopyrightText: 2022-present Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0

package logger

import (
	utilLogger "github.com/omec-project/util/logger"
	"go.uber.org/zap/zapcore"
)

var component = utilLogger.NewComponent("UPFAdapter")

var (
	AppLog  = component.Category("App")
	PfcpLog = component.Category("Pfcp")
	CfgLog  = component.Category("Config")
)

// SetLogLevel sets the runtime log level (panic|fatal|error|warn|info|debug)
func SetLogLevel(level zapcore.Level) {
	CfgLog.Infoln("set log level:", level)
	component.SetLevel(level)
}
