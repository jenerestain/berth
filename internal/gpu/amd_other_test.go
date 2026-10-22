// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package gpu

import "testing"

func TestAMDUnavailableOffLinux(t *testing.T) {
	if _, err := Open(Options{Backend: "amd"}); err == nil {
		t.Fatal("AMD enabled outside Linux")
	}
}
