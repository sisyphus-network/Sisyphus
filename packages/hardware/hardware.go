// Package hardware finds out what a machine has to offer: its processor,
// its memory, its graphics cards, and its other accelerators (NPUs, Edge
// TPUs and FPGAs). What it cannot find out it leaves
// unknown rather than guess, and it never fails: a node on a system it does
// not know how to look at is a node with less known about it.
package hardware

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Info is what a machine has.
type Info struct {
	// CPUModel is the processor's name, or empty if unknown.
	CPUModel string
	// MemoryBytes is how much memory the machine has, or zero if unknown.
	MemoryBytes uint64
	GPUs        []GPU
	// Accelerators are the machine's NPUs, other compute accelerators, Edge
	// TPUs and FPGAs.
	Accelerators []Accelerator
}

// GPU is a graphics card that can be computed on.
type GPU struct {
	Name        string
	MemoryBytes uint64
}

// These are how the machine is looked at. Tests put others in their place.
var (
	readFile = os.ReadFile
	run      = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}
	system = runtime.GOOS
	arch   = runtime.GOARCH
	// sysfs is the machine's file system from its root, where Linux lists
	// its devices.
	sysfs = os.DirFS("/")
)

// What Windows is asked for, through the shell it always has: the memory
// in bytes on one line, and the processor's name on the next.
const askWindows = "(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory; (Get-CimInstance Win32_Processor | Select-Object -First 1).Name"

// Detect looks at this machine. It takes a few seconds at most: a tool that
// does not answer promptly is taken to have nothing to say.
func Detect(ctx context.Context) Info {
	var info Info
	look, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Linux says what it has in two files. macOS and Windows each have a
	// tool to ask. On anything else the processor and memory stay unknown.
	switch system {
	case "darwin":
		if memory, err := run(look, "sysctl", "-n", "hw.memsize"); err == nil {
			info.MemoryBytes, _ = strconv.ParseUint(strings.TrimSpace(string(memory)), 10, 64)
		}
		if cpu, err := run(look, "sysctl", "-n", "machdep.cpu.brand_string"); err == nil {
			info.CPUModel = strings.TrimSpace(string(cpu))
		}
	case "windows":
		if said, err := run(look, "powershell", "-NoProfile", "-NonInteractive", "-Command", askWindows); err == nil {
			memory, cpu, _ := strings.Cut(strings.TrimSpace(string(said)), "\n")
			// Anything but a number first is the shell complaining.
			if bytes, err := strconv.ParseUint(strings.TrimSpace(memory), 10, 64); err == nil {
				info.MemoryBytes, info.CPUModel = bytes, strings.TrimSpace(cpu)
			}
		}
	default:
		if cpu, err := readFile("/proc/cpuinfo"); err == nil {
			info.CPUModel = cpuModel(string(cpu))
		}
		if memory, err := readFile("/proc/meminfo"); err == nil {
			info.MemoryBytes = memoryTotal(string(memory))
		}
	}
	// NVIDIA's tool is the same on every system that has one of its cards.
	if listed, err := run(look, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits"); err == nil {
		info.GPUs = nvidiaGPUs(string(listed))
	}
	// Other accelerators are looked for after, with time of their own: on
	// Windows that is a second shell to start.
	look, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info.Accelerators = accelerators(look, info.CPUModel)
	return info
}

// Offer returns what of a machine its owner offers to the pool: no more
// memory than maxMemory bytes, if that is not zero, and no more graphics
// cards than maxGPUs, if that is not negative.
func (i Info) Offer(maxMemory uint64, maxGPUs int) Info {
	if maxMemory > 0 && (i.MemoryBytes == 0 || i.MemoryBytes > maxMemory) {
		i.MemoryBytes = maxMemory
	}
	if maxGPUs >= 0 && len(i.GPUs) > maxGPUs {
		i.GPUs = i.GPUs[:maxGPUs]
	}
	return i
}

// cpuModel finds the processor's name in the text of /proc/cpuinfo.
func cpuModel(cpuinfo string) string {
	for line := range strings.Lines(cpuinfo) {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// memoryTotal finds the machine's memory in the text of /proc/meminfo.
func memoryTotal(meminfo string) uint64 {
	scanner := bufio.NewScanner(strings.NewReader(meminfo))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			kb, _ := strconv.ParseUint(fields[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

// nvidiaGPUs reads what nvidia-smi lists: a name and megabytes of memory
// for each card, one to a line.
func nvidiaGPUs(listed string) []GPU {
	var gpus []GPU
	for line := range strings.Lines(listed) {
		name, memory, ok := strings.Cut(strings.TrimSpace(line), ",")
		if !ok {
			continue
		}
		mb, _ := strconv.ParseUint(strings.TrimSpace(memory), 10, 64)
		gpus = append(gpus, GPU{Name: strings.TrimSpace(name), MemoryBytes: mb << 20})
	}
	return gpus
}
