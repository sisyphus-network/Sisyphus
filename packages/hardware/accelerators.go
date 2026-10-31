package hardware

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// The kinds of accelerator a machine may have besides its processor and
// graphics cards.
const (
	// KindNPU is a neural processing unit: Intel's NPU, AMD's XDNA (Ryzen
	// AI), Rockchip's NPU, Apple's Neural Engine and Qualcomm's Hexagon.
	KindNPU = "npu"
	// KindAccel is a compute accelerator that is not known to be an NPU:
	// a device of Linux's compute accelerator class whose driver is not
	// one of an NPU's, such as Qualcomm's Cloud AI 100 (qaic) or Habana's
	// Gaudi (habanalabs). The class holds any device that computes and is
	// not a graphics card, so what one of them is good for is not known.
	KindAccel = "accel"
	// KindEdgeTPU is a Google Coral Edge TPU, on PCIe or USB.
	KindEdgeTPU = "edgetpu"
	// KindFPGA is a field-programmable gate array: an AMD/Xilinx card run
	// through XRT, or one Linux's FPGA subsystem manages, as Intel's OPAE
	// cards are.
	KindFPGA = "fpga"
)

// Accelerator is a device besides the processor and graphics cards that
// work could be done on. What is not known of it is left empty.
type Accelerator struct {
	// Kind is one of KindNPU, KindAccel, KindEdgeTPU and KindFPGA.
	Kind string
	// Vendor is who makes it, as "Intel", "AMD", "Apple", "Xilinx",
	// "Google" or "Qualcomm", or as the system names them otherwise.
	Vendor string
	// Model is what the device is, as far as the system says.
	Model string
	// Driver is the operating system's driver for it, if it has one.
	Driver string
	// MemoryBytes is the memory on the device, or zero if unknown.
	MemoryBytes uint64
}

// String says what the accelerator is in a few words: its kind, maker and
// model, and its driver in brackets, such as "npu Intel NPU (Meteor Lake)
// [intel_vpu]". A model that starts with its maker's name is not given it
// twice.
func (a Accelerator) String() string {
	said := []string{a.Kind}
	if a.Vendor != "" && !strings.HasPrefix(a.Model, a.Vendor) {
		said = append(said, a.Vendor)
	}
	if a.Model != "" {
		said = append(said, a.Model)
	}
	if a.Driver != "" {
		said = append(said, "["+a.Driver+"]")
	}
	return strings.Join(said, " ")
}

// Labels returns what the accelerator lets a job ask for by name: its kind,
// and its kind and maker together where the maker is known, such as "npu"
// and "npu-intel".
func (a Accelerator) Labels() []string {
	labels := []string{a.Kind}
	if maker := slug(a.Vendor); maker != "" {
		labels = append(labels, a.Kind+"-"+maker)
	}
	return labels
}

// Labels returns what the machine's accelerators let a job ask for, each
// once and in order, or nothing for a machine with none.
func (i Info) Labels() []string {
	var labels []string
	for _, a := range i.Accelerators {
		labels = append(labels, a.Labels()...)
	}
	slices.Sort(labels)
	return slices.Compact(labels)
}

// slug is a maker's name as a label has it: lower case, letters and digits.
func slug(vendor string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return -1
	}, vendor)
}

// accelerators finds the machine's accelerators, on a system it knows how
// to look for them on. cpu is the processor's name, which on a Mac names the
// chip its Neural Engine is part of.
func accelerators(ctx context.Context, cpu string) []Accelerator {
	switch system {
	case "linux":
		return linuxAccelerators(sysfs)
	case "darwin":
		return appleNeuralEngine(ctx, cpu)
	case "windows":
		if said, err := run(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", askWindowsAccelerators); err == nil {
			return windowsAccelerators(said)
		}
	}
	return nil
}

// appleNeuralEngine is the Neural Engine every Apple silicon chip has. A
// daemon built for Intel and run by Rosetta is on one too, which the system
// says when asked.
func appleNeuralEngine(ctx context.Context, cpu string) []Accelerator {
	if arch != "arm64" {
		said, err := run(ctx, "sysctl", "-n", "hw.optional.arm64")
		if err != nil || strings.TrimSpace(string(said)) != "1" {
			return nil
		}
	}
	return []Accelerator{{Kind: KindNPU, Vendor: "Apple", Model: strings.TrimSpace(cpu + " Neural Engine")}}
}

// What Windows is asked for its NPUs: the devices of the class Windows 11
// gives them, each with its name, maker and driver service, as JSON. A
// system without the class lists nothing.
const askWindowsAccelerators = "Get-PnpDevice -Class ComputeAccelerator -PresentOnly -ErrorAction SilentlyContinue | Select-Object FriendlyName,Manufacturer,Service | ConvertTo-Json -Compress"

// windowsAccelerators reads what Windows lists of its compute accelerators:
// one device as an object, several as a list of them.
func windowsAccelerators(said []byte) []Accelerator {
	text := strings.TrimSpace(string(said))
	if strings.HasPrefix(text, "{") {
		text = "[" + text + "]"
	}
	var devices []struct{ FriendlyName, Manufacturer, Service string }
	if json.Unmarshal([]byte(text), &devices) != nil {
		return nil
	}
	var found []Accelerator
	for _, d := range devices {
		found = append(found, Accelerator{Kind: KindNPU, Vendor: maker(d.Manufacturer), Model: d.FriendlyName, Driver: d.Service})
	}
	return found
}

// maker is a manufacturer's name as Windows gives it, shortened to the name
// a label uses where it is one of those known.
func maker(manufacturer string) string {
	for _, known := range []struct{ in, is string }{
		{"Intel", "Intel"}, {"Advanced Micro Devices", "AMD"}, {"AMD", "AMD"},
		{"Qualcomm", "Qualcomm"}, {"Xilinx", "Xilinx"}, {"Google", "Google"},
	} {
		if strings.Contains(manufacturer, known.in) {
			return known.is
		}
	}
	return strings.TrimSpace(manufacturer)
}

// linuxAccelerators finds what Linux lists, in the tree root holds as / of
// the machine: its compute accelerators, its Edge TPUs and its FPGAs.
func linuxAccelerators(root fs.FS) []Accelerator {
	found := accelDevices(root)
	found = append(found, edgeTPUs(root)...)
	return append(found, fpgas(root)...)
}

// pciVendors are the makers, by PCI vendor ID, of the devices looked for.
var pciVendors = map[string]string{
	"8086": "Intel", "1022": "AMD", "10ee": "Xilinx", "1ac1": "Google",
	"17cb": "Qualcomm", "1da3": "Habana Labs",
}

// pciModels are the names of the PCI devices known by their IDs.
var pciModels = map[string]string{
	"8086:7d1d": "Intel NPU (Meteor Lake)",
	"8086:ad1d": "Intel NPU (Arrow Lake)",
	"8086:643e": "Intel NPU (Lunar Lake)",
	"8086:b03e": "Intel NPU (Panther Lake)",
	"1022:1502": "AMD XDNA NPU (Phoenix, Hawk Point)",
	"1022:17f0": "AMD XDNA 2 NPU (Strix, Krackan)",
	"1ac1:089a": "Coral Edge TPU (PCIe)",
}

// driverVendors are the makers of devices whose drivers only theirs use.
var driverVendors = map[string]string{
	"intel_vpu": "Intel", "amdxdna": "AMD", "rocket": "Rockchip", "qaic": "Qualcomm",
	"habanalabs": "Habana Labs", "xocl": "Xilinx", "xclmgmt": "Xilinx",
}

// device is what Linux says of the device behind a class entry dir, in its
// device/uevent file: its driver, and its maker and model if it is on PCI.
func device(root fs.FS, dir string) (driver, vendor, model string) {
	said, err := fs.ReadFile(root, path.Join(dir, "device", "uevent"))
	if err != nil {
		return "", "", ""
	}
	var id string
	scanner := bufio.NewScanner(strings.NewReader(string(said)))
	for scanner.Scan() {
		key, value, _ := strings.Cut(scanner.Text(), "=")
		switch key {
		case "DRIVER":
			driver = value
		case "PCI_ID":
			id = strings.ToLower(value)
		}
	}
	vendorID, _, _ := strings.Cut(id, ":")
	vendor = pciVendors[vendorID]
	if vendor == "" {
		vendor = driverVendors[driver]
	}
	model = pciModels[id]
	if model == "" && id != "" {
		model = "PCI device " + id
	}
	return driver, vendor, model
}

// entries are the names in a directory of root that start with prefix.
func entries(root fs.FS, dir, prefix string) []string {
	listed, _ := fs.ReadDir(root, dir)
	var names []string
	for _, entry := range listed {
		if strings.HasPrefix(entry.Name(), prefix) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// npuDrivers are the drivers of Linux's compute accelerator class that
// drive only NPUs: Intel's NPU (intel_vpu), AMD's XDNA (amdxdna) and
// Rockchip's NPU (rocket).
var npuDrivers = map[string]bool{"intel_vpu": true, "amdxdna": true, "rocket": true}

// accelDevices are the devices of Linux's compute accelerator class. The
// class is for any device that computes and is not a graphics card, so
// only those an NPU's driver drives are NPUs; the rest are KindAccel.
func accelDevices(root fs.FS) []Accelerator {
	var found []Accelerator
	for _, name := range entries(root, "sys/class/accel", "accel") {
		driver, vendor, model := device(root, path.Join("sys/class/accel", name))
		kind := KindAccel
		if npuDrivers[driver] {
			kind = KindNPU
		}
		found = append(found, Accelerator{Kind: kind, Vendor: vendor, Model: model, Driver: driver})
	}
	return found
}

// coralUSB are the USB IDs a Coral USB Accelerator has, before its firmware
// is loaded and after.
var coralUSB = map[string]string{"1a6e:089a": "Coral USB Accelerator", "18d1:9302": "Coral USB Accelerator"}

// edgeTPUs are the Coral Edge TPUs: those on PCIe, which the apex driver
// gives a /dev/apex_N each, and those on USB, known by their IDs.
func edgeTPUs(root fs.FS) []Accelerator {
	var found []Accelerator
	for _, name := range entries(root, "dev", "apex_") {
		driver, _, model := device(root, path.Join("sys/class/apex", name))
		found = append(found, Accelerator{Kind: KindEdgeTPU, Vendor: "Google", Model: cmp.Or(model, "Coral Edge TPU (PCIe)"), Driver: cmp.Or(driver, "apex")})
	}
	for _, name := range entries(root, "sys/bus/usb/devices", "") {
		dir := path.Join("sys/bus/usb/devices", name)
		vendor, _ := fs.ReadFile(root, path.Join(dir, "idVendor"))
		product, _ := fs.ReadFile(root, path.Join(dir, "idProduct"))
		id := strings.TrimSpace(string(vendor)) + ":" + strings.TrimSpace(string(product))
		if model, ok := coralUSB[id]; ok {
			found = append(found, Accelerator{Kind: KindEdgeTPU, Vendor: "Google", Model: model})
		}
	}
	return found
}

// fpgas are the FPGAs: AMD/Xilinx cards XRT drives, each with a render node
// of the xocl driver for its user function and a /dev/xclmgmtN for its
// management one, and those Linux's FPGA subsystem manages, as Intel's OPAE
// cards and Xilinx's Zynq are. A card's user function is counted where it
// has one, so a card is not counted twice.
func fpgas(root fs.FS) []Accelerator {
	var found []Accelerator
	for _, name := range entries(root, "sys/class/drm", "renderD") {
		if driver, vendor, model := device(root, path.Join("sys/class/drm", name)); driver == "xocl" {
			found = append(found, Accelerator{Kind: KindFPGA, Vendor: vendor, Model: model, Driver: driver})
		}
	}
	if len(found) == 0 {
		for range entries(root, "dev", "xclmgmt") {
			found = append(found, Accelerator{Kind: KindFPGA, Vendor: "Xilinx", Driver: "xclmgmt"})
		}
	}
	managers := entries(root, "sys/class/fpga_manager", "")
	for _, name := range managers {
		dir := path.Join("sys/class/fpga_manager", name)
		said, _ := fs.ReadFile(root, path.Join(dir, "name"))
		model := strings.TrimSpace(string(said))
		driver, vendor, _ := device(root, dir)
		found = append(found, Accelerator{Kind: KindFPGA, Vendor: cmp.Or(vendor, fpgaVendor(model+" "+driver)), Model: model, Driver: driver})
	}
	// A region without a manager is an FPGA all the same.
	if len(managers) == 0 {
		for _, name := range entries(root, "sys/class/fpga_region", "") {
			driver, vendor, model := device(root, path.Join("sys/class/fpga_region", name))
			found = append(found, Accelerator{Kind: KindFPGA, Vendor: cmp.Or(vendor, fpgaVendor(driver)), Model: model, Driver: driver})
		}
	}
	return found
}

// fpgaVendor is who makes an FPGA, as its manager's name or driver says.
func fpgaVendor(said string) string {
	said = strings.ToLower(said)
	for _, known := range []struct{ in, is string }{
		{"xilinx", "Xilinx"}, {"zynq", "Xilinx"}, {"versal", "Xilinx"},
		{"dfl", "Intel"}, {"intel", "Intel"}, {"altera", "Intel"}, {"socfpga", "Intel"},
	} {
		if strings.Contains(said, known.in) {
			return known.is
		}
	}
	return ""
}
