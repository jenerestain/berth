// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	units "github.com/T-Crypt/berth/contrib"
)

// liveUnitDir is where the systemd user units live.
func liveUnitDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// unitPath is one unit's file in the user's systemd directory.
func (o serviceOps) unitPath(name string) (string, error) {
	dir, err := o.unitDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".service"), nil
}

// renderUnit is the unit file's text with the binary the user runs in the
// place of the one the contrib file names: the contrib files stay the
// source of the unit's words, and only its path moves.
func renderUnit(name, exe string) (string, error) {
	text, ok := units.Text(name)
	if !ok {
		return "", fmt.Errorf("no unit text for %q", name)
	}
	out := ""
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "ExecStart="); ok {
			if _, args, ok := strings.Cut(rest, " "); ok {
				line = "ExecStart=" + exe + " " + args
			} else {
				line = "ExecStart=" + exe
			}
		}
		out += line + "\n"
	}
	return out, nil
}

// serviceInstall writes the unit files, then asks systemd to know them and
// start them. A unit file that differs is kept beside it as .bak, like
// berth skill install does. With dryRun nothing is written or run.
func serviceInstall(ctx context.Context, ops serviceOps, unitsList []Unit, dryRun bool) (*serviceDoc, *Error) {
	doc := &serviceDoc{DryRun: dryRun, Units: make([]serviceUnit, 0, len(unitsList)), Commands: []string{}}
	for _, u := range unitsList {
		target, err := ops.unitPath(u.Name)
		if err != nil {
			return nil, Systemf("find %s's unit file: %v", u.Name, err)
		}
		text, err := renderUnit(u.Name, u.Exec[0])
		if err != nil {
			return nil, Systemf("%s: %v", u.Name, err)
		}
		if u.Name == "berthd" {
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "ExecStart=") {
					lines[i] = "ExecStart=" + strings.Join(u.Exec, " ")
				}
			}
			text = strings.Join(lines, "\n")
		}
		row := serviceUnit{Unit: u.Name, Target: target, Content: text}
		if dryRun {
			row.Action = "would be written"
		} else {
			row.Action = "unchanged"
			old, readErr := os.ReadFile(target)
			if readErr != nil && !os.IsNotExist(readErr) {
				return nil, Systemf("read %s: %v", target, readErr)
			}
			if readErr != nil || string(old) != text {
				if readErr == nil {
					bak := target + ".bak"
					if err := os.Rename(target, bak); err != nil {
						return nil, Systemf("keep the previous copy of %s: %v", target, err)
					}
					row.BackedUp = bak
				}
				if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
					return nil, Systemf("write %s: %v", target, err)
				}
				row.Action = "written"
			}
		}
		doc.Units = append(doc.Units, row)
	}
	commands := []string{"systemctl --user daemon-reload"}
	for _, u := range unitsList {
		commands = append(commands, "systemctl --user enable --now "+u.Name)
	}
	doc.Commands = commands
	if dryRun {
		return doc, nil
	}
	for _, line := range commands {
		fields := strings.Fields(line)
		if out, err := ops.run(ctx, fields[0], fields[1:]...); err != nil {
			return nil, Systemf("%s failed:\n%s", line, strings.TrimSpace(out))
		}
	}
	return doc, nil
}

// serviceUninstall asks systemd to stop the units, removes their files and
// tells systemd about it. A unit that was never installed is reported, not
// an error.
func serviceUninstall(ctx context.Context, ops serviceOps, unitsList []Unit, dryRun bool) (*serviceDoc, *Error) {
	doc := &serviceDoc{DryRun: dryRun, Units: make([]serviceUnit, 0, len(unitsList)), Commands: []string{}}
	for _, u := range unitsList {
		target, err := ops.unitPath(u.Name)
		if err != nil {
			return nil, Systemf("find %s's unit file: %v", u.Name, err)
		}
		row := serviceUnit{Unit: u.Name, Target: target}
		if !fileExists(target) {
			row.Action = "not installed"
			doc.Units = append(doc.Units, row)
			continue
		}
		if dryRun {
			row.Action = "would be removed"
		} else {
			cmd := "systemctl --user disable --now " + u.Name
			if out, err := ops.run(ctx, "systemctl", "--user", "disable", "--now", u.Name); err != nil {
				return nil, Systemf("%s failed:\n%s", cmd, strings.TrimSpace(out))
			}
			doc.Commands = append(doc.Commands, cmd, "rm "+target)
			if err := os.Remove(target); err != nil {
				return nil, Systemf("remove %s: %v", target, err)
			}
			row.Action = "removed"
		}
		doc.Units = append(doc.Units, row)
	}
	if !dryRun {
		cmd := "systemctl --user daemon-reload"
		if out, err := ops.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return nil, Systemf("%s failed:\n%s", cmd, strings.TrimSpace(out))
		}
		doc.Commands = append(doc.Commands, cmd)
	}
	return doc, nil
}

// serviceStatus reports each unit: installed says its file is where the
// install writes it, running says systemctl calls it active.
func serviceStatus(ctx context.Context, ops serviceOps, unitsList []Unit) (*serviceDoc, *Error) {
	doc := &serviceDoc{Units: make([]serviceUnit, 0, len(unitsList)), Commands: []string{}}
	for _, u := range unitsList {
		target, err := ops.unitPath(u.Name)
		if err != nil {
			return nil, Systemf("find %s's unit file: %v", u.Name, err)
		}
		row := serviceUnit{Unit: u.Name, Target: target, Installed: fileExists(target)}
		out, err := ops.run(ctx, "systemctl", "--user", "is-active", u.Name)
		if err != nil && !inactiveOut(out) {
			return nil, Systemf("systemctl --user is-active %s failed:\n%s", u.Name, strings.TrimSpace(out))
		}
		row.Running = strings.TrimSpace(out) == "active"
		doc.Units = append(doc.Units, row)
	}
	return doc, nil
}

// inactiveOut is systemctl is-active answering that the unit is not active:
// it exits non-zero for every state but active and still prints the state,
// so only a state it could not report at all is a failure.
func inactiveOut(out string) bool {
	switch strings.TrimSpace(out) {
	case "inactive", "failed", "unknown":
		return true
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
