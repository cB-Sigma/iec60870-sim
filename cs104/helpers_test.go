// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package cs104_test

import (
	"testing"
	"time"
)

// eventually polls f until it holds or timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}
