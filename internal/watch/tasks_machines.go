package watch

import (
	"sort"
	"time"
)

// TaskMachine is inventory, not a scheduling promise. Busy and offline machines
// remain visible, with the same resource occupancy rules used by Ready.
type TaskMachine struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Group           string    `json:"group"`
	Host            string    `json:"host"`
	Port            int       `json:"port"`
	ResourceKey     string    `json:"resourceKey"`
	Enabled         bool      `json:"enabled"`
	Status          string    `json:"status"`
	UpdatedAt       time.Time `json:"updatedAt"`
	Ready           bool      `json:"ready"`
	Reason          string    `json:"reason"`
	TaskIDs         []string  `json:"taskIds"`
	ReservationIDs  []string  `json:"reservationIds"`
	ContinuationIDs []string  `json:"continuationIds"`
}

func (m *TaskManager) Machines() []TaskMachine {
	c, states := m.store.Snapshot(), m.monitor.Snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	ready := map[string]bool{}
	for _, machine := range m.readyLocked(c, states, "", "") {
		ready[machine.ID] = true
	}
	resources := map[string]string{}
	targets := map[string]ReservationAllocation{}
	for _, machine := range c.Machines {
		resources[machine.ID] = machineResource(machine)
		targets[machine.ID] = machineAllocation(machine)
	}
	out := make([]TaskMachine, 0, len(c.Machines))
	for _, machine := range c.Machines {
		state := states[machine.ID]
		item := TaskMachine{ID: machine.ID, Name: machine.Name, Group: machine.Group, Host: machine.Host, Port: machine.Port,
			ResourceKey: resources[machine.ID], Enabled: machine.Enabled, Status: state.Status,
			UpdatedAt: state.UpdatedAt, Ready: ready[machine.ID], TaskIDs: []string{}, ReservationIDs: []string{}}
		for _, job := range m.jobs {
			if job.FinishedAt == nil && (resourcesConflict(taskAllocation(job), targets[machine.ID]) || resourcesConflict(targets[job.SelectedMachineID], targets[machine.ID])) {
				item.TaskIDs = append(item.TaskIDs, job.ID)
			}
		}
		for _, reservation := range m.reservations {
			if reservation.ReleasedAt == nil {
				for _, a := range reservationAllocations(reservation) {
					if resourcesConflict(a, targets[machine.ID]) {
						item.ReservationIDs = append(item.ReservationIDs, reservation.ID)
						break
					}
				}
			}
		}
		for id, continuation := range m.continuations {
			if !continuationOccupied(continuation) {
				continue
			}
			for _, allocation := range continuation.Allocations {
				if resourcesConflict(continuationAllocation(allocation), targets[machine.ID]) {
					item.ContinuationIDs = append(item.ContinuationIDs, id)
					break
				}
			}
		}
		sort.Strings(item.ContinuationIDs)
		sort.Strings(item.TaskIDs)
		sort.Strings(item.ReservationIDs)
		switch {
		case !machine.Enabled:
			item.Reason = "已停用"
		case len(item.TaskIDs) > 0:
			item.Reason = "任务占用"
		case len(item.ReservationIDs) > 0:
			item.Reason = "已被预约"
		case len(item.ContinuationIDs) > 0:
			item.Reason = "实验等待单暂留或持有"
		case state.Status != "online" && state.Status != "partial":
			item.Reason = "监控未在线"
		case m.cleanup.Busy[item.ResourceKey]:
			item.Reason = "残留进程诊断或清理中"
		case !item.Ready:
			item.Reason = "监控状态已过期"
		default:
			item.Reason = "ready"
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
