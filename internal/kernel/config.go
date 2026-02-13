package kernel

import "github.com/selemilka/hivekernel/internal/process"

// Config holds system-wide configuration for the kernel.
type Config struct {
	// VPS identity
	NodeName string // e.g. "vps1"
	ListenAddr string // gRPC listen address, e.g. "unix:///tmp/hivekernel.sock" or ":50051"

	// Kernel process settings
	KernelUser string // default "root"

	// Default resource limits for spawned processes
	DefaultLimits process.ResourceLimits

	// IPC settings
	MessageAgingFactor float64 // priority aging factor, default 0.1
}

// DefaultConfig returns sensible defaults for single-VPS development.
func DefaultConfig() Config {
	return Config{
		NodeName:   "vps1",
		ListenAddr: "unix:///tmp/hivekernel.sock",
		KernelUser: "root",
		DefaultLimits: process.ResourceLimits{
			MaxTokensPerHour:   100000,
			MaxContextTokens:   128000,
			MaxChildren:        10,
			MaxConcurrentTasks: 5,
			TimeoutSeconds:     300,
			MaxTokensTotal:     1000000,
		},
		MessageAgingFactor: 0.1,
	}
}
