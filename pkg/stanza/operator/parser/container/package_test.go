// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// The atomicLimiter goroutine in helper.Cache is stopped by op.Stop().
		// Some table-driven tests build operators in closures; the limiter
		// is cleaned up but goleak sees it between test completion and its own check.
		goleak.IgnoreTopFunction("github.com/open-telemetry/opentelemetry-collector-contrib/pkg/stanza/operator/helper.(*atomicLimiter).init.func1.1"),
	)
}
