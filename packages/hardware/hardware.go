// Package hardware finds out what a machine has to offer: its processor,
// its memory, and its graphics cards. What it cannot find out it leaves
// unknown rather than guess, and it never fails: a node on a system it does
// not know how to look at is a node with less known about it.
package hardware

import (
	"bufio"
	"context"
	"os"
	"os/exec"
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
)

// Detect looks at this machine. It takes a moment at most: a tool that
// does not answer promptly is taken to have nothing to say.
func Detect(ctx context.Context) Info {
	var info Info
	// Linux says what it has in two files. Elsewhere they are not there,
	// and the processor and memory stay unknown.
	if cpu, err := readFile("/proc/cpuinfo"); err == nil {
		info.CPUModel = cpuModel(string(cpu))
	}
	if memory, err := readFile("/proc/meminfo"); err == nil {
		info.MemoryBytes = memoryTotal(string(memory))
	}
	// NVIDIA's tool is the same on every system that has one of its cards.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if listed, err := run(ctx, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits"); err == nil {
		info.GPUs = nvidiaGPUs(string(listed))
	}
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
