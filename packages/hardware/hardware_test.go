package hardware

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

const (
	someCPUInfo = "processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) CPU E5-2660 0 @ 2.20GHz\nprocessor\t: 1\nmodel name\t: Intel(R) Xeon(R) CPU E5-2660 0 @ 2.20GHz\n"
	someMemInfo = "MemTotal:       264044560 kB\nMemFree:        80149604 kB\nHugePages_Total:       0\n"
	someGPUs    = "NVIDIA GeForce RTX 4090, 24564\nTesla T4, 15360\n"
)

// machine puts a pretend machine in place of the real one for a test.
func machine(t *testing.T, files map[string]string, tool string, toolErr error) {
	t.Helper()
	oldRead, oldRun := readFile, run
	t.Cleanup(func() { readFile, run = oldRead, oldRun })
	readFile = func(name string) ([]byte, error) {
		if content, ok := files[name]; ok {
			return []byte(content), nil
		}
		return nil, errors.New("no such file")
	}
	run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "nvidia-smi" || len(args) != 2 {
			t.Errorf("ran %s %v", name, args)
		}
		return []byte(tool), toolErr
	}
}

func TestDetectingWhatALinuxMachineHas(t *testing.T) {
	machine(t, map[string]string{"/proc/cpuinfo": someCPUInfo, "/proc/meminfo": someMemInfo}, someGPUs, nil)
	got := Detect(context.Background())
	want := Info{
		CPUModel: "Intel(R) Xeon(R) CPU E5-2660 0 @ 2.20GHz", MemoryBytes: 264044560 << 10,
		GPUs: []GPU{{"NVIDIA GeForce RTX 4090", 24564 << 20}, {"Tesla T4", 15360 << 20}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detected %+v, want %+v", got, want)
	}
}

func TestWhatCannotBeFoundOutIsLeftUnknown(t *testing.T) {
	// A system with neither file and no NVIDIA tool.
	machine(t, nil, "", errors.New("executable file not found"))
	if got := Detect(context.Background()); !reflect.DeepEqual(got, Info{}) {
		t.Errorf("on a machine that says nothing: %+v", got)
	}
	// Files that are there and say nothing of use, and a tool that babbles.
	machine(t, map[string]string{"/proc/cpuinfo": "processor: 0\nflags: fpu vme\n", "/proc/meminfo": "MemFree: 12 kB\nMemTotal: lots\n"}, "no devices were found\n\n", nil)
	if got := Detect(context.Background()); got.CPUModel != "" || got.MemoryBytes != 0 || len(got.GPUs) != 0 {
		t.Errorf("on a machine that says nothing of use: %+v", got)
	}
}

func TestThisMachineCanBeLookedAt(t *testing.T) {
	// Whatever this is running on, looking at it does no harm.
	got := Detect(context.Background())
	t.Logf("this machine: %+v", got)
}

func TestAnOwnerOffersNoMoreThanTheyChoose(t *testing.T) {
	has := Info{CPUModel: "some", MemoryBytes: 64 << 30, GPUs: []GPU{{"a", 1}, {"b", 2}}}
	for name, tt := range map[string]struct {
		maxMemory uint64
		maxGPUs   int
		memory    uint64
		gpus      int
	}{
		"everything":         {0, -1, 64 << 30, 2},
		"some of the memory": {16 << 30, -1, 16 << 30, 2},
		"a limit above it":   {128 << 30, 5, 64 << 30, 2},
		"one card":           {0, 1, 64 << 30, 1},
		"no cards":           {0, 0, 64 << 30, 0},
	} {
		if got := has.Offer(tt.maxMemory, tt.maxGPUs); got.MemoryBytes != tt.memory || len(got.GPUs) != tt.gpus || got.CPUModel != "some" {
			t.Errorf("%s: offers %+v", name, got)
		}
	}
	// Where the machine's memory is unknown, the owner's figure stands.
	if got := (Info{}).Offer(8<<30, -1); got.MemoryBytes != 8<<30 {
		t.Errorf("with memory unknown and 8 GiB offered: %+v", got)
	}
	if len(has.GPUs) != 2 {
		t.Error("offering less changed what the machine has")
	}
}
