// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gpu

import "testing"

func TestValidateSelection(t *testing.T) {
	for _, backend := range []string{"", "auto", "nvidia", "amd"} {
		if err := ValidateSelection(backend, 0); err != nil {
			t.Fatal(err)
		}
	}
	for _, opts := range []Options{{Backend: "cuda"}, {Backend: "amd", Index: -1}} {
		if _, err := Open(opts); err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
	p, err := Open(Options{Backend: "nvidia"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(NvidiaSMI); !ok {
		t.Fatalf("one-shot NVIDIA probe changed: %T", p)
	}
}
