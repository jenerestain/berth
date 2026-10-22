// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedUnit(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name+".service")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRenderUnit(t *testing.T) {
	text, err := renderUnit("berthd", "/opt/berth/berth")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "ExecStart=/opt/berth/berth daemon --gpu 0 --poll 2s") {
		t.Errorf("the binary path is substituted into ExecStart:\n%s", text)
	}
	if strings.Contains(text, "%h") {
		t.Errorf("the home expansion survives the render, which it must not:\n%s", text)
	}
	if !strings.Contains(text, "Before=llama-swap.service") {
		t.Errorf("the rest of the contrib file is kept:\n%s", text)
	}
	if _, err := renderUnit("nope", "/opt/berth/berth"); err == nil {
		t.Error("an unknown unit name is an error")
	}
}

func TestServiceInstallLinux(t *testing.T) {
	dir := t.TempDir()
	const exe = "/opt/berth/berth"
	// A previous, different berthd unit is kept as .bak.
	old := seedUnit(t, dir, "berthd", "ExecStart=/old/berth daemon\n")
	ops, calls := fakeOps(t, dir, nil)

	doc, e := serviceInstall(context.Background(), ops, serviceUnits(exe, true, false), false)
	if e != nil {
		t.Fatalf("install failed: %v", e)
	}
	if len(doc.Units) != 2 {
		t.Fatalf("two units, got %+v", doc.Units)
	}
	if doc.Units[0].Action != "written" || doc.Units[0].BackedUp != old+".bak" {
		t.Errorf("the differing file is written and its previous copy kept, got %+v", doc.Units[0])
	}
	if doc.Units[1].Action != "written" || doc.Units[1].BackedUp != "" {
		t.Errorf("a fresh file is written with no backup, got %+v", doc.Units[1])
	}

	wrote, err := os.ReadFile(filepath.Join(dir, "berthd.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wrote), "ExecStart="+exe+" daemon --gpu 0 --poll 2s") {
		t.Errorf("the file carries the user's binary:\n%s", wrote)
	}
	if _, err := os.Stat(old + ".bak"); err != nil {
		t.Errorf("the previous copy is kept: %v", err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now berthd",
		"systemctl --user enable --now berth-gateway",
	}
	if len(*calls) != 3 {
		t.Fatalf("three systemctl calls, got %v", *calls)
	}
	for i, line := range want {
		if (*calls)[i].String() != line {
			t.Errorf("call %d = %q, want %q", i, (*calls)[i], line)
		}
	}
}

func TestServiceInstallLinuxUnchanged(t *testing.T) {
	dir := t.TempDir()
	const exe = "/opt/berth/berth"
	text, _ := renderUnit("berthd", exe)
	path := seedUnit(t, dir, "berthd", text)
	ops, calls := fakeOps(t, dir, nil)

	doc, e := serviceInstall(context.Background(), ops, serviceUnits(exe, false, false), false)
	if e != nil {
		t.Fatalf("install failed: %v", e)
	}
	if doc.Units[0].Action != "unchanged" || doc.Units[0].BackedUp != "" {
		t.Errorf("an identical file is left alone, got %+v", doc.Units[0])
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("no .bak for an unchanged file: %v", err)
	}
	if len(*calls) != 2 {
		t.Errorf("daemon-reload and one enable, got %v", *calls)
	}
}

func TestServiceInstallLinuxDryRun(t *testing.T) {
	dir := t.TempDir()
	const exe = "/opt/berth/berth"
	ops, calls := fakeOps(t, dir, nil)

	doc, e := serviceInstall(context.Background(), ops, serviceUnits(exe, true, true), true)
	if e != nil {
		t.Fatalf("dry run failed: %v", e)
	}
	if !doc.DryRun {
		t.Errorf("the doc says it is a dry run")
	}
	for _, u := range doc.Units {
		if u.Action != "would be written" {
			t.Errorf("dry run says would be written, got %q for %s", u.Action, u.Unit)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("a dry run runs no command, got %v", *calls)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a dry run writes no file, got %v", entries)
	}
	if len(doc.Commands) != 4 {
		t.Errorf("the dry run names the commands it would run, got %v", doc.Commands)
	}
}

func TestServiceInstallLinuxAMDSelection(t *testing.T) {
	ops, calls := fakeOps(t, t.TempDir(), nil)
	doc, e := serviceInstall(context.Background(), ops, serviceUnitsForGPU("/opt/berth", false, false, "amd", 10), true)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(doc.Units[0].Content, "ExecStart=/opt/berth daemon --gpu 10 --poll 2s --gpu-backend amd") {
		t.Fatal(doc.Units[0].Content)
	}
	if len(*calls) != 0 {
		t.Fatal("dry run changed services")
	}
}

func TestServiceInstallLinuxCommandFails(t *testing.T) {
	dir := t.TempDir()
	const exe = "/opt/berth/berth"
	ops, _ := fakeOps(t, dir, func(a argv) (string, error) {
		if a.name == "systemctl" && a.args[0] == "--user" && a.args[1] == "daemon-reload" {
			return "System has not been booted with systemd", fmt.Errorf("exit status 1")
		}
		return "", nil
	})

	_, e := serviceInstall(context.Background(), ops, serviceUnits(exe, false, false), false)
	if e == nil {
		t.Fatal("a failing systemctl is a system failure")
	}
	if e.Code != ExitSystem {
		t.Errorf("a system failure exits 2, got %d", e.Code)
	}
	if !strings.Contains(e.Msg, "System has not been booted with systemd") {
		t.Errorf("the failure carries the command's output, got %q", e.Msg)
	}
}

func TestServiceUninstallLinux(t *testing.T) {
	dir := t.TempDir()
	seedUnit(t, dir, "berthd", "x")
	seedUnit(t, dir, "berth-gateway", "x")
	// berth-harbor was never installed.
	ops, calls := fakeOps(t, dir, nil)

	doc, e := serviceUninstall(context.Background(), ops, allUnits(), false)
	if e != nil {
		t.Fatalf("uninstall failed: %v", e)
	}
	byName := map[string]serviceUnit{}
	for _, u := range doc.Units {
		byName[u.Unit] = u
	}
	if byName["berthd"].Action != "removed" || byName["berth-gateway"].Action != "removed" {
		t.Errorf("installed units are removed, got %+v", doc.Units)
	}
	if byName["berth-harbor"].Action != "not installed" {
		t.Errorf("a missing unit is reported, not an error, got %+v", byName["berth-harbor"])
	}
	if _, err := os.Stat(filepath.Join(dir, "berthd.service")); !os.IsNotExist(err) {
		t.Errorf("the unit file is gone: %v", err)
	}
	// The rm lines are the removals, shown in the report; the commands
	// run are the systemctl ones.
	wantCalls := []string{
		"systemctl --user disable --now berthd",
		"systemctl --user disable --now berth-gateway",
		"systemctl --user daemon-reload",
	}
	if len(*calls) != len(wantCalls) {
		t.Fatalf("%d systemctl calls, got %v", len(wantCalls), *calls)
	}
	for i, line := range wantCalls {
		fields := strings.Fields(line)
		if (*calls)[i].name != fields[0] || !sameArgs((*calls)[i].args, fields[1:]) {
			t.Errorf("call %d = %s, want %s", i, (*calls)[i], line)
		}
	}
	for _, want := range []string{
		"rm " + filepath.Join(dir, "berthd.service"),
		"rm " + filepath.Join(dir, "berth-gateway.service"),
		"systemctl --user daemon-reload",
	} {
		found := false
		for _, line := range doc.Commands {
			if line == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the report names %q, got %v", want, doc.Commands)
		}
	}
}

func TestServiceStatusLinux(t *testing.T) {
	dir := t.TempDir()
	seedUnit(t, dir, "berthd", "x")
	seedUnit(t, dir, "berth-harbor", "x")
	ops, _ := fakeOps(t, dir, func(a argv) (string, error) {
		if a.name == "systemctl" && a.args[0] == "--user" && a.args[1] == "is-active" {
			switch a.args[2] {
			case "berthd":
				return "active\n", nil
			case "berth-gateway", "berth-harbor":
				return "inactive\n", errors.New("exit status 3")
			}
		}
		return "", errors.New("unexpected call " + a.String())
	})

	doc, e := serviceStatus(context.Background(), ops, allUnits())
	if e != nil {
		t.Fatalf("status failed: %v", e)
	}
	byName := map[string]serviceUnit{}
	for _, u := range doc.Units {
		byName[u.Unit] = u
	}
	if !byName["berthd"].Installed || !byName["berthd"].Running {
		t.Errorf("berthd is installed and running, got %+v", byName["berthd"])
	}
	if byName["berth-gateway"].Installed || byName["berth-gateway"].Running {
		t.Errorf("berth-gateway is absent, got %+v", byName["berth-gateway"])
	}
	if !byName["berth-harbor"].Installed || byName["berth-harbor"].Running {
		t.Errorf("berth-harbor is installed and stopped, got %+v", byName["berth-harbor"])
	}
}

func TestServiceStatusLinuxSystemctlMissing(t *testing.T) {
	dir := t.TempDir()
	ops, _ := fakeOps(t, dir, func(a argv) (string, error) {
		return "", errors.New(`exec: "systemctl": executable file not found in $PATH`)
	})

	_, e := serviceStatus(context.Background(), ops, allUnits())
	if e == nil || e.Code != ExitSystem {
		t.Errorf("a missing systemctl is a system failure, got %+v", e)
	}
}
