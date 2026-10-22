// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
	"testing"
)

type argv struct {
	name string
	args []string
}

func (a argv) String() string { return a.name + " " + strings.Join(a.args, " ") }

// fakeOps is serviceOps with a recorded runner: a test never reaches
// systemctl or schtasks, and the platform's files land in dir. respond
// answers each call; nil answers nothing.
func fakeOps(t *testing.T, dir string, respond func(argv) (string, error)) (serviceOps, *[]argv) {
	t.Helper()
	if respond == nil {
		respond = func(argv) (string, error) { return "", nil }
	}
	var calls []argv
	ops := serviceOps{
		run: func(ctx context.Context, name string, args ...string) (string, error) {
			call := argv{name, args}
			calls = append(calls, call)
			return respond(call)
		},
		unitDir: func() (string, error) { return dir, nil },
	}
	return ops, &calls
}

func TestServiceUnits(t *testing.T) {
	const exe = "/opt/berth/berth"
	// No unit carries --listen: the address is a berth.toml key the
	// command reads at start, so a port change needs a restart.
	cases := []struct {
		gateway bool
		harbor  bool
		want    []Unit
	}{
		{
			gateway: false, harbor: false,
			want: []Unit{{
				Name:        "berthd",
				Exec:        []string{exe, "daemon", "--gpu", "0", "--poll", "2s"},
				Description: "berth - GPU lease broker (one card, many inference engines)",
			}},
		},
		{
			gateway: true, harbor: true,
			want: []Unit{
				{
					Name:        "berthd",
					Exec:        []string{exe, "daemon", "--gpu", "0", "--poll", "2s"},
					Description: "berth - GPU lease broker (one card, many inference engines)",
				},
				{
					Name:        "berth-gateway",
					Exec:        []string{exe, "gateway"},
					After:       []string{"berthd"},
					Description: "berth gateway - one port that starts engines on demand under a lease",
				},
				{
					Name:        "berth-harbor",
					Exec:        []string{exe, "harbor"},
					After:       []string{"berthd"},
					Description: "berth harbor - web UI for the local berthd",
				},
			},
		},
	}
	for _, c := range cases {
		got := serviceUnits(exe, c.gateway, c.harbor)
		if len(got) != len(c.want) {
			t.Errorf("serviceUnits(gateway %v, harbor %v) = %d units, want %d", c.gateway, c.harbor, len(got), len(c.want))
			continue
		}
		for i, want := range c.want {
			if got[i].Name != want.Name || !sameArgs(got[i].Exec, want.Exec) || got[i].Description != want.Description {
				t.Errorf("unit %d = %+v, want %+v", i, got[i], want)
				continue
			}
			if want.Name != "berthd" && !sameArgs(got[i].After, []string{"berthd"}) {
				t.Errorf("%s comes up after the node, got %v", want.Name, got[i].After)
			}
		}
	}

	if u := serviceUnits(exe, true, false)[1]; u.Name != "berth-gateway" {
		t.Errorf("--gateway selects the gateway, got %s", u.Name)
	}
	if u := serviceUnits(exe, false, true)[1]; u.Name != "berth-harbor" {
		t.Errorf("--harbor selects the harbor, got %s", u.Name)
	}
}

func TestServiceGPUSelection(t *testing.T) {
	units := serviceUnitsForGPU("/opt/berth", true, true, "amd", 10)
	if got := strings.Join(units[0].Exec, " "); got != "/opt/berth daemon --gpu 10 --poll 2s --gpu-backend amd" {
		t.Fatal(got)
	}
	if strings.Contains(strings.Join(units[1].Exec, " "), "gpu") || strings.Contains(strings.Join(units[2].Exec, " "), "gpu") {
		t.Fatal("GPU flags leaked to gateway or harbor")
	}
}

func TestResolveListen(t *testing.T) {
	cases := []struct {
		flag, key, def, want string
	}{
		{"", "", "127.0.0.1:9798", "127.0.0.1:9798"},                            // nothing set: the default
		{"", "0.0.0.0:50980", "127.0.0.1:9798", "0.0.0.0:50980"},                // the key
		{"127.0.0.1:8080", "0.0.0.0:50980", "127.0.0.1:9798", "127.0.0.1:8080"}, // the flag wins
		{"127.0.0.1:8080", "", "127.0.0.1:9798", "127.0.0.1:8080"},
	}
	for _, c := range cases {
		if got := resolveListen(c.flag, c.key, c.def); got != c.want {
			t.Errorf("resolveListen(%q, %q, %q) = %q, want %q", c.flag, c.key, c.def, got, c.want)
		}
	}
}

func sameArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
