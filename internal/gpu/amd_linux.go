// Copyright 2026 Trevin Tindall (T-Crypt)
// SPDX-License-Identifier: Apache-2.0

package gpu

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// AMDGPU observes RDNA3/RDNA4 discrete cards and Phoenix APUs without opening a DRM device or
// creating a HIP/Vulkan context. SysRoot is /sys/class/drm, ProcRoot is /proc.
// Card indices are the N in cardN, not HIP ordinals. APU VRAM/GTT stay separate.
type AMDGPU struct {
	Index    int
	SysRoot  string
	ProcRoot string
	MaxAge   time.Duration
}

func (a AMDGPU) roots() (string, string) {
	sys, proc := a.SysRoot, a.ProcRoot
	if sys == "" {
		sys = "/sys/class/drm"
	}
	if proc == "" {
		proc = "/proc"
	}
	return sys, proc
}

func openAMD(o Options) (Probe, error) {
	age := 3 * o.Interval
	if age <= 0 {
		age = 10 * time.Second
	}
	return &AMDGPU{Index: o.Index, SysRoot: o.SysRoot, ProcRoot: o.ProcRoot, MaxAge: age}, nil
}

func hasAMD(o Options) (bool, error) {
	a := AMDGPU{SysRoot: o.SysRoot}
	cards, err := a.cards()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return len(cards) > 0, err
}

type amdCard struct {
	Device
	path string
}

// rdnaDiscrete deliberately recognises discrete GC IPs, not every 11.x GPU:
// Phoenix (11.0.1/4) and the 11.5 APUs have a different memory model.
func rdnaDiscrete(major, minor, rev uint64) string {
	if minor != 0 {
		return ""
	}
	if major == 11 && (rev == 0 || rev == 2 || rev == 3) {
		return "RDNA3"
	}
	if major == 12 && (rev == 0 || rev == 1) {
		return "RDNA4"
	}
	return ""
}

func (a AMDGPU) cards() ([]amdCard, error) {
	sys, _ := a.roots()
	entries, err := os.ReadDir(sys)
	if err != nil {
		return nil, fmt.Errorf("AMD inventory: %w", err)
	}
	var cards []amdCard
	for _, e := range entries {
		n, ok := strings.CutPrefix(e.Name(), "card")
		idx, err := strconv.Atoi(n)
		if !ok || err != nil || idx < 0 {
			continue
		}
		p := filepath.Join(sys, e.Name(), "device")
		uevent, err := os.ReadFile(filepath.Join(p, "uevent"))
		if err != nil {
			continue
		}
		fields := keyValues(string(uevent), "=")
		if fields["DRIVER"] != "amdgpu" {
			continue
		}
		ip := filepath.Join(p, "ip_discovery/die/0/GC/0")
		major, e1 := readUint(filepath.Join(ip, "major"))
		minor, e2 := readUint(filepath.Join(ip, "minor"))
		rev, e3 := readUint(filepath.Join(ip, "revision"))
		generation := rdnaDiscrete(major, minor, rev)
		apu := major == 11 && minor == 0 && rev == 1 // Phoenix, including Radeon 780M
		if apu {
			generation = "RDNA3 APU"
		}
		if e1 != nil || e2 != nil || e3 != nil || generation == "" {
			continue
		}
		bus, err := pciAddress(fields["PCI_SLOT_NAME"])
		if err != nil {
			return nil, fmt.Errorf("AMD card%d PCI identity: %w", idx, err)
		}
		name := strings.TrimSpace(readText(filepath.Join(p, "product_name")))
		if name == "" {
			name = fmt.Sprintf("AMD %s (%s)", generation, fields["PCI_ID"])
		}
		cards = append(cards, amdCard{Device: Device{Index: idx, Name: name, BusID: bus, APU: apu,
			Util: -1, Temp: -1, Power: -1, PowerLimit: -1, Fan: -1, ClockSM: -1, ClockMem: -1, PCIeGen: -1, PCIeWidth: -1}, path: p})
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Index < cards[j].Index })
	return cards, nil
}

// Read reports inaccessible same-user PIDs explicitly. Foreign users'
// unreadable processes remain in the driver baseline. Card totals are read
// after fdinfo so delayed frees stay counted.
func (a AMDGPU) Read(ctx context.Context) (snap Snapshot, err error) {
	snap.Guarded, snap.MaxAge, snap.At = true, a.MaxAge, time.Now()
	if snap.MaxAge <= 0 {
		snap.MaxAge = 10 * time.Second
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: AMD: %w", ErrUnsafeRead, err)
		}
	}()
	cards, err := a.cards()
	if err != nil {
		return snap, err
	}
	var selected *amdCard
	for i := range cards {
		if cards[i].Index == a.Index {
			selected = &cards[i]
		}
	}
	if selected == nil {
		return snap, fmt.Errorf("card%d is not an available RDNA3/RDNA4 discrete or Phoenix APU amdgpu card", a.Index)
	}
	snap.Procs, snap.Unobserved, err = a.processes(ctx, selected.BusID, selected.APU)
	if err != nil {
		return snap, err
	}
	for i := range cards {
		if err := ctx.Err(); err != nil {
			return snap, err
		}
		c := &cards[i]
		c.Total, err = readUint(filepath.Join(c.path, "mem_info_vram_total"))
		if err != nil {
			return snap, err
		}
		c.Used, err = readUint(filepath.Join(c.path, "mem_info_vram_used"))
		if err != nil {
			return snap, err
		}
		if c.Total == 0 || c.Used > c.Total {
			return snap, fmt.Errorf("card%d invalid VRAM reading: used %d, total %d", c.Index, c.Used, c.Total)
		}
		c.Free = c.Total - c.Used
		if c.APU {
			c.GTTTotal, err = readUint(filepath.Join(c.path, "mem_info_gtt_total"))
			if err != nil {
				return snap, err
			}
			c.GTTUsed, err = readUint(filepath.Join(c.path, "mem_info_gtt_used"))
			if err != nil {
				return snap, err
			}
			if c.GTTTotal == 0 || c.GTTUsed > c.GTTTotal {
				return snap, fmt.Errorf("card%d invalid GTT reading: used %d, capacity %d", c.Index, c.GTTUsed, c.GTTTotal)
			}
		}
		amdTelemetry(&c.Device, c.path)
		snap.Devices = append(snap.Devices, c.Device)
	}
	snap.Index, snap.Name, snap.Total, snap.Used, snap.Free = selected.Index, selected.Name, selected.Total, selected.Used, selected.Free
	snap.APU, snap.GTTTotal, snap.GTTUsed = selected.APU, selected.GTTTotal, selected.GTTUsed
	var accounted, shared uint64
	for _, p := range snap.Procs {
		if p.Used > snap.Used-accounted {
			return snap, fmt.Errorf("process VRAM exceeds card usage; memory changed during scan, retrying")
		}
		accounted += p.Used
		if p.GTT > snap.GTTUsed-shared {
			return snap, fmt.Errorf("process GTT exceeds driver usage; memory changed during scan, retrying")
		}
		shared += p.GTT
	}
	return snap, nil
}

func keyValues(s, sep string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(line, sep); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// pciAddress normalises both DRM's four-digit and NVIDIA's eight-digit domains.
func pciAddress(s string) (string, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid PCI address %q", s)
	}
	tail := strings.Split(parts[2], ".")
	if len(tail) != 2 || (len(parts[0]) != 4 && len(parts[0]) != 8) || len(parts[1]) != 2 || len(tail[0]) != 2 || len(tail[1]) != 1 {
		return "", fmt.Errorf("invalid PCI address %q", s)
	}
	p := []string{parts[0], parts[1], tail[0], tail[1]}
	var n [4]uint64
	for i, bits := range []int{32, 8, 5, 3} {
		v, err := strconv.ParseUint(p[i], 16, bits)
		if err != nil {
			return "", fmt.Errorf("invalid PCI address %q", s)
		}
		n[i] = v
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", n[0], n[1], n[2], n[3]), nil
}

type drmClient struct {
	bus, id string
	used    uint64
	gtt     uint64
}

func parseDRM(s, want string, apu bool) (drmClient, bool, error) {
	f := keyValues(s, ":")
	if f["drm-driver"] != "amdgpu" {
		return drmClient{}, false, nil
	}
	bus, err := pciAddress(f["drm-pdev"])
	if err != nil {
		return drmClient{}, false, err
	}
	if bus != want {
		return drmClient{}, false, nil
	}
	id, err := strconv.ParseUint(f["drm-client-id"], 10, 64)
	if err != nil {
		return drmClient{}, false, fmt.Errorf("missing/invalid drm-client-id")
	}
	used, err := privateResident(f, "vram")
	if err != nil {
		return drmClient{}, false, err
	}
	var gtt uint64
	if apu {
		gtt, err = privateResident(f, "gtt")
		if err != nil {
			return drmClient{}, false, err
		}
	}
	return drmClient{bus: bus, id: strconv.FormatUint(id, 10), used: used, gtt: gtt}, true, nil
}

// Aliases are never additive; requested buffers and CPU heap figures do not
// represent resident GPU storage. Shared objects stay in each pool's baseline.
func privateResident(f map[string]string, domain string) (uint64, error) {
	mem, ok := f["drm-resident-"+domain]
	if !ok {
		mem, ok = f["drm-memory-"+domain]
	}
	if !ok {
		return 0, fmt.Errorf("missing resident %s counter", domain)
	}
	used, err := drmBytes(mem)
	if err != nil {
		return 0, err
	}
	// Shared objects can be charged to several clients. There is no buffer
	// identity here with which to deduplicate them: attribute only the private
	// lower bound and leave shared storage in the card's baseline instead.
	if shared, ok := f["drm-shared-"+domain]; ok {
		n, err := drmBytes(shared)
		if err != nil {
			return 0, err
		}
		used -= min(used, n)
	}
	return used, nil
}

func drmBytes(s string) (uint64, error) {
	f := strings.Fields(s)
	if len(f) < 1 || len(f) > 2 {
		return 0, fmt.Errorf("invalid DRM memory %q", s)
	}
	n, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return 0, err
	}
	mul := uint64(1)
	if len(f) == 2 {
		switch f[1] {
		case "KiB":
			mul = 1024
		case "MiB":
			mul = MiB
		case "B":
		default:
			return 0, fmt.Errorf("unknown DRM memory unit %q", f[1])
		}
	}
	if n > math.MaxUint64/mul {
		return 0, fmt.Errorf("DRM memory overflow")
	}
	return n * mul, nil
}

type clientOwner struct {
	proc Proc
	used uint64
	gtt  uint64
}

func (a AMDGPU) processes(ctx context.Context, bus string, apu bool) ([]Proc, []int, error) {
	_, root := a.roots()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	clients := map[string]clientOwner{}
	var unobserved []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		p := filepath.Join(root, e.Name())
		fds, err := os.ReadDir(filepath.Join(p, "fdinfo"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if errors.Is(err, os.ErrPermission) {
			info, statErr := os.Stat(p)
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			if statErr == nil {
				if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Geteuid()) {
					continue
				}
			}
			unobserved = append(unobserved, pid)
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("pid %d fdinfo: %w", pid, err)
		}
		var samples []drmClient
		before, statErr := procStart(p)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return nil, nil, statErr
		}
		inaccessible := false
		for _, fd := range fds {
			if _, err := strconv.Atoi(fd.Name()); err != nil {
				continue
			}
			b, err := os.ReadFile(filepath.Join(p, "fdinfo", fd.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if errors.Is(err, os.ErrPermission) {
				inaccessible = true
				break
			}
			if err != nil {
				return nil, nil, fmt.Errorf("pid %d fdinfo: %w", pid, err)
			}
			client, ok, err := parseDRM(string(b), bus, apu)
			if err != nil {
				return nil, nil, fmt.Errorf("pid %d DRM accounting: %w", pid, err)
			}
			if ok {
				samples = append(samples, client)
			}
		}
		if inaccessible {
			unobserved = append(unobserved, pid)
			continue
		}
		if len(samples) == 0 {
			continue
		}
		after, err := procStart(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if before != after {
			return nil, nil, fmt.Errorf("pid %d reused during GPU scan", pid)
		}
		proc := Proc{PID: pid, Name: strings.TrimSpace(readText(filepath.Join(p, "comm"))), Start: before}
		for _, s := range samples {
			key := s.bus + "/" + s.id
			old, ok := clients[key]
			if !ok {
				clients[key] = clientOwner{proc: proc, used: s.used, gtt: s.gtt}
				continue
			}
			// Duplicated descriptors (also across fork) describe one client.
			// Choose a deterministic PID, and the larger observed residency.
			if proc.PID < old.proc.PID {
				old.proc = proc
			}
			old.used = max(old.used, s.used)
			old.gtt = max(old.gtt, s.gtt)
			clients[key] = old
		}
	}
	byPID := map[int]Proc{}
	for _, c := range clients {
		if c.used == 0 && c.gtt == 0 {
			continue
		} // an open, empty context is not a memory holder
		p, ok := byPID[c.proc.PID]
		if !ok {
			p = c.proc
		}
		if math.MaxUint64-p.Used < c.used {
			return nil, nil, fmt.Errorf("pid %d VRAM sum overflow", p.PID)
		}
		p.Used += c.used
		if math.MaxUint64-p.GTT < c.gtt {
			return nil, nil, fmt.Errorf("pid %d GTT sum overflow", p.PID)
		}
		p.GTT += c.gtt
		byPID[p.PID] = p
	}
	out := make([]Proc, 0, len(byPID))
	for _, p := range byPID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	sort.Ints(unobserved)
	return out, unobserved, nil
}

func procStart(path string) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(path, "stat"))
	if err != nil {
		return 0, err
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, fmt.Errorf("invalid process stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0, fmt.Errorf("short process stat")
	}
	n, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid process start time %q", f[19])
	}
	return n, nil
}

func readText(path string) string { b, _ := os.ReadFile(path); return string(b) }
func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
}

func optionalNumber(path string, scale float64) float64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return -1
	}
	return v / scale
}

func amdTelemetry(d *Device, path string) {
	d.Util = optionalNumber(filepath.Join(path, "gpu_busy_percent"), 1)
	hwmons, _ := filepath.Glob(filepath.Join(path, "hwmon/hwmon*"))
	for _, hw := range hwmons {
		if strings.TrimSpace(readText(filepath.Join(hw, "name"))) != "amdgpu" {
			continue
		}
		d.Temp = optionalNumber(filepath.Join(hw, "temp1_input"), 1000)
		d.Power = optionalNumber(filepath.Join(hw, "power1_average"), 1e6)
		if d.Power < 0 {
			d.Power = optionalNumber(filepath.Join(hw, "power1_input"), 1e6)
		}
		d.PowerLimit = optionalNumber(filepath.Join(hw, "power1_cap"), 1e6)
		d.ClockSM = optionalNumber(filepath.Join(hw, "freq1_input"), 1e6)
		d.ClockMem = optionalNumber(filepath.Join(hw, "freq2_input"), 1e6)
		// fan1_input is RPM, not a percentage. PWM is the driver's 0..255
		// fan-control duty, not actual fan speed: leave Fan unknown.
		break
	}
	d.PCIeWidth = optionalNumber(filepath.Join(path, "current_link_width"), 1)
	f := strings.Fields(readText(filepath.Join(path, "current_link_speed")))
	if len(f) > 0 {
		switch f[0] {
		case "2.5":
			d.PCIeGen = 1
		case "5.0":
			d.PCIeGen = 2
		case "8.0":
			d.PCIeGen = 3
		case "16.0":
			d.PCIeGen = 4
		case "32.0":
			d.PCIeGen = 5
		case "64.0":
			d.PCIeGen = 6
		}
	}
}
