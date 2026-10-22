// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package gpu

import "fmt"

func hasAMD(Options) (bool, error) { return false, nil }

func openAMD(Options) (Probe, error) {
	return nil, fmt.Errorf("AMD GPU probing requires Linux; AMD Windows support is not implemented")
}
