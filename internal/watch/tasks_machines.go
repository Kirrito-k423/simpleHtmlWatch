package watch

import (
	"sort"
	"strings"
	"time"
)

// TaskMachine is inventory, not a scheduling promise. Busy and offline machines
// remain visible, with the same resource occupancy rules used by Ready.
type TaskMachine struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Group          string    `json:"group"`
	Host           string    `json:"host"`
	ResourceKey    string    `json:"resourceKey"`
	Enabled        bool      `json:"enabled"`
	Status         string    `json:"status"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Ready          bool      `json:"ready"`
	Reason         string    `json:"reason"`
	TaskIDs        []string  `json:"taskIds"`
	ReservationIDs []string  `json:"reservationIds"`
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
	for _, machine := range c.Machines {
		resources[machine.ID] = machineResource(machine)
	}
	out := make([]TaskMachine, 0, len(c.Machines))
	for _, machine := range c.Machines {
		state := states[machine.ID]
		item := TaskMachine{ID: machine.ID, Name: machine.Name, Group: machine.Group, Host: machine.Host,
			ResourceKey: resources[machine.ID], Enabled: machine.Enabled, Status: state.Status,
			UpdatedAt: state.UpdatedAt, Ready: ready[machine.ID], TaskIDs: []string{}, ReservationIDs: []string{}}
		for _, job := range m.jobs {
			if job.FinishedAt == nil && (job.SelectedMachineID == machine.ID || strings.EqualFold(job.Host, machine.Host) || resources[job.SelectedMachineID] == item.ResourceKey) {
				item.TaskIDs = append(item.TaskIDs, job.ID)
			}
		}
		for _, reservation := range m.reservations {
			if reservation.ReleasedAt == nil && (reservation.MachineID == machine.ID || strings.EqualFold(reservation.Host, machine.Host) || reservation.ResourceKey == item.ResourceKey) {
				item.ReservationIDs = append(item.ReservationIDs, reservation.ID)
			}
		}
		sort.Strings(item.TaskIDs)
		sort.Strings(item.ReservationIDs)
		switch {
		case !machine.Enabled:
			item.Reason = "已停用"
		case len(item.TaskIDs) > 0:
			item.Reason = "任务占用"
		case len(item.ReservationIDs) > 0:
			item.Reason = "已被预约"
		case state.Status != "online" && state.Status != "partial":
			item.Reason = "监控未在线"
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
