package watch

import (
	"net"
	"strings"
)

func machineAllocation(h Machine) ReservationAllocation {
	return ReservationAllocation{h.ID, machineResource(h), h.Host, h.Port}
}

func taskAllocation(job *TaskJob) ReservationAllocation {
	return ReservationAllocation{job.SelectedMachineID, job.ResourceKey, job.Host, job.Port}
}

func continuationAllocation(a ContinuationAllocation) ReservationAllocation {
	return ReservationAllocation{a.MachineID, a.ResourceKey, a.Host, a.Port}
}

func resourceHost(host string) (string, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	ip := net.ParseIP(host)
	if host == "localhost" || ip != nil && ip.IsLoopback() {
		return "localhost", true
	}
	return host, false
}

func physicalResource(key string) bool {
	id, ok := strings.CutPrefix(key, "physical:")
	return ok && identifier.MatchString(id)
}

// A loopback host is a forwarding entry point, not a physical node. Only
// distinct, explicit physical identities at distinct valid ports can bypass
// host-wide exclusion. Legacy host-only holds remain conservative.
func resourcesConflict(a, b ReservationAllocation) bool {
	if a.MachineID != "" && a.MachineID == b.MachineID || a.ResourceKey != "" && a.ResourceKey == b.ResourceKey {
		return true
	}
	ah, loopback := resourceHost(a.Host)
	bh, _ := resourceHost(b.Host)
	if ah == "" || ah != bh {
		return false
	}
	return !loopback || a.Port == b.Port || a.Port < 1 || a.Port > 65535 || b.Port < 1 || b.Port > 65535 || !physicalResource(a.ResourceKey) || !physicalResource(b.ResourceKey)
}

func conflictsWithAny(target ReservationAllocation, held []ReservationAllocation) bool {
	for _, other := range held {
		if resourcesConflict(target, other) {
			return true
		}
	}
	return false
}
