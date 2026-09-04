package agent

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func heartbeatSnapshot(
	state *State,
	bootID string,
	acceptedRevision int64,
	clockOffset time.Duration,
) *agentv1.Heartbeat {
	uptime, uptimeOK := readUptime()
	freeBytes, storageOK := freeBytes(state.Directory())
	checks := []*agentv1.HealthCheck{
		{Name: "local_state", Healthy: storageOK, ErrorCode: errorCode(storageOK, "STATE_STORAGE_UNAVAILABLE")},
		{Name: "system_clock", Healthy: clockOffset <= maxClockOffset && clockOffset >= -maxClockOffset, ErrorCode: errorCode(clockOffset <= maxClockOffset && clockOffset >= -maxClockOffset, "CLOCK_SKEW")},
		{Name: "uptime", Healthy: uptimeOK, ErrorCode: errorCode(uptimeOK, "UPTIME_UNAVAILABLE")},
	}
	return &agentv1.Heartbeat{
		BootId: bootID, UptimeSeconds: uptime,
		AcceptedRevision: acceptedRevision, StateFreeBytes: freeBytes,
		ClockOffsetMs: clockOffset.Milliseconds(), HealthChecks: checks,
		LocalTime: timestamppb.Now(),
	}
}

func inventorySnapshot(
	state *State,
	version string,
	clockOffset time.Duration,
) *agentv1.InventoryReport {
	free, _ := freeBytes(state.Directory())
	return &agentv1.InventoryReport{
		CapturedAt: timestamppb.Now(), Kernel: readTrimmed("/proc/sys/kernel/osrelease", 256),
		OsRelease: readOSRelease(), CpuArch: runtime.GOARCH, AgentVersion: version,
		Containerized:  isContainerized(),
		AvailableBytes: map[string]uint64{"agent_state": free},
		ClockOffsetMs:  clockOffset.Milliseconds(),
		Capabilities:   []string{"certificate_rotation_v1", "desired_state_v1", "inventory_v1"},
	}
}

const maxClockOffset = 2 * time.Minute

func readUptime() (uint64, bool) {
	value := readTrimmed("/proc/uptime", 64)
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return uint64(seconds), true
}

func freeBytes(path string) (uint64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, false
	}
	return stat.Bavail * uint64(stat.Bsize), true
}

func readOSRelease() string {
	value := readTrimmed("/etc/os-release", 4096)
	for _, line := range strings.Split(value, "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return truncate(strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"'`), 256)
		}
	}
	return "linux"
}

func isContainerized() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	value := readTrimmed("/proc/1/cgroup", 4096)
	return strings.Contains(value, "docker") || strings.Contains(value, "containerd") ||
		strings.Contains(value, "kubepods")
}

func readTrimmed(path string, limit int) string {
	value, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return truncate(strings.TrimSpace(string(value)), limit)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func errorCode(healthy bool, code string) string {
	if healthy {
		return ""
	}
	return code
}
