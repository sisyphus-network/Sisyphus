package hardware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLinuxListsItsAccelerators(t *testing.T) {
	for name, want := range map[string][]Accelerator{
		"intel-npu": {{Kind: KindNPU, Vendor: "Intel", Model: "Intel NPU (Meteor Lake)", Driver: "intel_vpu"}},
		"amd-xdna":  {{Kind: KindNPU, Vendor: "AMD", Model: "AMD XDNA 2 NPU (Strix, Krackan)", Driver: "amdxdna"}},
		// Devices of the accelerator class an NPU's driver does not drive
		// are not taken for NPUs: Qualcomm's Cloud AI 100, of a maker it
		// knows only by its PCI ID; one that says nothing; and Habana's
		// Gaudi. Rockchip's NPU, on no PCI bus, is one.
		"other-accel": {
			{Kind: KindAccel, Vendor: "Qualcomm", Model: "PCI device 17cb:a100", Driver: "qaic"},
			{Kind: KindNPU, Vendor: "Rockchip", Driver: "rocket"},
			{Kind: KindAccel},
			{Kind: KindAccel, Vendor: "Habana Labs", Model: "PCI device 1da3:1020", Driver: "habanalabs"},
		},
		"coral-pcie": {
			{Kind: KindEdgeTPU, Vendor: "Google", Model: "Coral Edge TPU (PCIe)", Driver: "apex"},
			{Kind: KindEdgeTPU, Vendor: "Google", Model: "Coral Edge TPU (PCIe)", Driver: "apex"},
		},
		// Before its firmware is loaded and after, among other USB devices.
		"coral-usb": {
			{Kind: KindEdgeTPU, Vendor: "Google", Model: "Coral USB Accelerator"},
			{Kind: KindEdgeTPU, Vendor: "Google", Model: "Coral USB Accelerator"},
		},
		// A card's user function, beside a graphics card's render node and
		// the card's own management function, which is the same card.
		"xilinx-xrt": {{Kind: KindFPGA, Vendor: "Xilinx", Model: "PCI device 10ee:5001", Driver: "xocl"}},
		// Cards whose user functions have no driver loaded.
		"xilinx-mgmt": {{Kind: KindFPGA, Vendor: "Xilinx", Driver: "xclmgmt"}, {Kind: KindFPGA, Vendor: "Xilinx", Driver: "xclmgmt"}},
		// An OPAE card's manager, and its region, which is the same card.
		"intel-opae": {{Kind: KindFPGA, Vendor: "Intel", Model: "DFL FME FPGA Manager", Driver: "dfl-fme-mgr"}},
		"zynq": {
			{Kind: KindFPGA, Vendor: "Xilinx", Model: "Xilinx ZynqMP FPGA Manager"},
			{Kind: KindFPGA, Model: "Lattice iCE40 FPGA Manager"},
		},
		"fpga-regions": {{Kind: KindFPGA, Vendor: "Intel", Driver: "dfl-fme-region"}, {Kind: KindFPGA}},
		"nothing":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			if got := linuxAccelerators(os.DirFS(filepath.Join("testdata", name))); !reflect.DeepEqual(got, want) {
				t.Errorf("found %+v, want %+v", got, want)
			}
		})
	}
}

func TestDetectingALinuxMachinesNPU(t *testing.T) {
	machine(t, map[string]string{"/proc/cpuinfo": "model name\t: Intel(R) Core(TM) Ultra 7 155H\n"}, "", errors.New("executable file not found"))
	sysfs = os.DirFS(filepath.Join("testdata", "intel-npu"))
	got := Detect(context.Background())
	want := Info{CPUModel: "Intel(R) Core(TM) Ultra 7 155H", Accelerators: []Accelerator{{Kind: KindNPU, Vendor: "Intel", Model: "Intel NPU (Meteor Lake)", Driver: "intel_vpu"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detected %+v, want %+v", got, want)
	}
	if labels := got.Labels(); !reflect.DeepEqual(labels, []string{"npu", "npu-intel"}) {
		t.Errorf("labels %v", labels)
	}
}

func TestAMacOnAppleSiliconHasANeuralEngine(t *testing.T) {
	ane := []Accelerator{{Kind: KindNPU, Vendor: "Apple", Model: "Apple M2 Neural Engine"}}
	for name, tt := range map[string]struct {
		arch    string
		answers map[string]string
		want    []Accelerator
	}{
		"arm64":                 {"arm64", nil, ane},
		"an Intel build on one": {"amd64", map[string]string{"sysctl hw.optional.arm64": "1\n"}, ane},
		"an Intel Mac":          {"amd64", map[string]string{"sysctl hw.optional.arm64": "0\n"}, nil},
		"an older Intel Mac":    {"amd64", nil, nil},
	} {
		answers := map[string]string{"sysctl machdep.cpu.brand_string": "Apple M2\n"}
		for k, v := range tt.answers {
			answers[k] = v
		}
		elsewhere(t, "darwin", answers)
		arch = tt.arch
		if got := Detect(context.Background()).Accelerators; !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: found %+v, want %+v", name, got, tt.want)
		}
	}
}

func TestWindowsListsItsNPUs(t *testing.T) {
	for name, tt := range map[string]struct {
		said string
		want []Accelerator
	}{
		"one": {`{"FriendlyName":"Intel(R) AI Boost","Manufacturer":"Intel Corporation","Service":"npu"}`,
			[]Accelerator{{Kind: KindNPU, Vendor: "Intel", Model: "Intel(R) AI Boost", Driver: "npu"}}},
		"several": {"[" +
			`{"FriendlyName":"Snapdragon(R) X Elite - X1E80100 - Qualcomm(R) Hexagon(TM) NPU","Manufacturer":"Qualcomm Technologies, Inc.","Service":"qcnspmcdm"},` +
			`{"FriendlyName":"NPU Compute Accelerator Device","Manufacturer":"Advanced Micro Devices, Inc.","Service":"kipudrv"},` +
			`{"FriendlyName":"Mystery NPU","Manufacturer":" Someone Else ","Service":null}` + "]\r\n",
			[]Accelerator{
				{Kind: KindNPU, Vendor: "Qualcomm", Model: "Snapdragon(R) X Elite - X1E80100 - Qualcomm(R) Hexagon(TM) NPU", Driver: "qcnspmcdm"},
				{Kind: KindNPU, Vendor: "AMD", Model: "NPU Compute Accelerator Device", Driver: "kipudrv"},
				{Kind: KindNPU, Vendor: "Someone Else", Model: "Mystery NPU"},
			}},
		"none":            {"", nil},
		"a shell's error": {"Get-PnpDevice : not recognized\r\n", nil},
	} {
		elsewhere(t, "windows", map[string]string{"powershell " + askWindowsAccelerators: tt.said})
		if got := Detect(context.Background()).Accelerators; !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: found %+v, want %+v", name, got, tt.want)
		}
	}
}

func TestOtherSystemsHaveNoAcceleratorsFound(t *testing.T) {
	// Whatever is in the tree, a system other than Linux does not look there.
	for _, name := range []string{"freebsd", "openbsd", "plan9"} {
		elsewhere(t, name, nil)
		sysfs = os.DirFS(filepath.Join("testdata", "intel-npu"))
		if got := Detect(context.Background()).Accelerators; got != nil {
			t.Errorf("on %s: %+v", name, got)
		}
	}
}

// A compute accelerator that is not known to be an NPU is not labelled as
// one, so that a job that needs an NPU is not sent to it.
func TestOnlyAnNPUsDriverMakesAnAcceleratorAnNPU(t *testing.T) {
	got := Info{Accelerators: linuxAccelerators(os.DirFS(filepath.Join("testdata", "other-accel")))}.Labels()
	if want := []string{"accel", "accel-habanalabs", "accel-qualcomm", "npu", "npu-rockchip"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels %v, want %v", got, want)
	}
}

func TestAcceleratorsGiveLabels(t *testing.T) {
	for _, tt := range []struct {
		has  []Accelerator
		want []string
	}{
		{nil, nil},
		{[]Accelerator{{Kind: KindNPU, Vendor: "Apple"}}, []string{"npu", "npu-apple"}},
		{[]Accelerator{{Kind: KindAccel, Vendor: "Habana Labs"}, {Kind: KindAccel}}, []string{"accel", "accel-habanalabs"}},
		{[]Accelerator{{Kind: KindEdgeTPU, Vendor: "Google"}, {Kind: KindEdgeTPU, Vendor: "Google"}}, []string{"edgetpu", "edgetpu-google"}},
		{[]Accelerator{{Kind: KindFPGA, Vendor: "Xilinx"}, {Kind: KindFPGA, Vendor: "Intel"}, {Kind: KindNPU, Vendor: "AMD"}}, []string{"fpga", "fpga-intel", "fpga-xilinx", "npu", "npu-amd"}},
		{[]Accelerator{{Kind: KindFPGA, Vendor: "X-1 & Co."}}, []string{"fpga", "fpga-x1co"}},
	} {
		if got := (Info{Accelerators: tt.has}).Labels(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%+v has labels %v, want %v", tt.has, got, tt.want)
		}
	}
}

func TestAnAcceleratorSaysWhatItIs(t *testing.T) {
	for _, tt := range []struct {
		a    Accelerator
		want string
	}{
		{Accelerator{Kind: KindNPU, Vendor: "Intel", Model: "Intel NPU (Meteor Lake)", Driver: "intel_vpu"}, "npu Intel NPU (Meteor Lake) [intel_vpu]"},
		{Accelerator{Kind: KindNPU, Vendor: "Qualcomm", Model: "PCI device 17cb:a100", Driver: "qaic"}, "npu Qualcomm PCI device 17cb:a100 [qaic]"},
		{Accelerator{Kind: KindFPGA, Vendor: "Xilinx", Driver: "xclmgmt"}, "fpga Xilinx [xclmgmt]"},
		{Accelerator{Kind: KindNPU}, "npu"},
	} {
		if got := tt.a.String(); got != tt.want {
			t.Errorf("%+v says %q, want %q", tt.a, got, tt.want)
		}
	}
}
