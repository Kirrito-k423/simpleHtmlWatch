package watch

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ResourceRequirements struct {
	Group             string            `json:"group,omitempty"`
	MachineIDs        []string          `json:"machineIds,omitempty"`
	MachineCount      int               `json:"machineCount"`
	Model             string            `json:"model"`
	DevicesPerMachine int               `json:"devicesPerMachine"`
	MinFreeHBMMB      int64             `json:"minFreeHbmMB"`
	MaxUtilization    float64           `json:"maxUtilization"`
	MaxSampleAge      int               `json:"maxSampleAgeSeconds"`
	Labels            map[string]string `json:"labels,omitempty"`
}

type npuDevice struct {
	ID        string
	Model     string
	Util      float64
	FreeHBMMB int64
}

var memoryPair = regexp.MustCompile(`([0-9]+)\s*/\s*([0-9]+)`)

// Parse current per-device facts. The historical machine mean deliberately is
// not used: one busy chip or an unrecognised process table vetoes allocation.
func parseNPUResources(output string) ([]npuDevice, error) {
	nameColumn, utilColumn, hbmColumn := -1, -1, -1
	current, model := "", ""
	models, cards, seen := map[string]string{}, map[string]bool{}, map[string]bool{}
	devices := []npuDevice{}
	processTable, noProcesses, processRow, inProcesses := false, false, false, false
	for _, line := range strings.Split(output, "\n") {
		cells := tableCells(line)
		lower := strings.ToLower(line)
		if strings.Contains(lower, "process id") {
			processTable, inProcesses = true, true
			continue
		}
		if inProcesses {
			if strings.Contains(lower, "no running process") {
				noProcesses = true
			} else if len(cells) > 1 {
				for _, cell := range cells {
					if _, err := strconv.ParseUint(strings.TrimSpace(cell), 10, 64); err == nil {
						processRow = true
					}
				}
			}
			continue
		}
		if len(cells) == 0 {
			continue
		}
		if strings.Contains(line, "NPU") && strings.Contains(line, "Health") {
			for i, cell := range cells {
				if strings.Contains(cell, "Name") {
					nameColumn = i
				}
			}
			continue
		}
		header := false
		for i, cell := range cells {
			if strings.Contains(cell, "AICore(%)") || strings.Contains(cell, "NPU Util(%)") {
				utilColumn, header = i, true
			}
			if strings.Contains(cell, "HBM-Usage(MB)") {
				hbmColumn = i
			}
		}
		if header || nameColumn < 0 || utilColumn < 0 || hbmColumn < 0 || nameColumn >= len(cells) {
			continue
		}
		ids, names := strings.Fields(cells[0]), strings.Fields(cells[nameColumn])
		if nameColumn == 0 && len(names) > 1 {
			names = names[1:]
		}
		if len(ids) > 0 && len(names) > 0 {
			_, idErr := strconv.Atoi(ids[0])
			_, nameErr := strconv.Atoi(names[0])
			if idErr == nil && nameErr != nil {
				if names[0] == "NA" || names[0] == "N/A" || models[ids[0]] != "" {
					return nil, errors.New("npu_model_invalid")
				}
				current, model = ids[0], names[0]
				models[current], cards[current] = model, false
				continue
			}
		}
		if current == "" || utilColumn >= len(cells) || hbmColumn >= len(cells) {
			continue
		}
		values := strings.Fields(cells[utilColumn])
		if len(values) == 0 {
			return nil, errors.New("npu_utilization_unknown")
		}
		util, err := strconv.ParseFloat(strings.TrimSuffix(values[0], "%"), 64)
		if err != nil || math.IsNaN(util) || math.IsInf(util, 0) || util < 0 || util > 100 {
			return nil, errors.New("npu_utilization_unknown")
		}
		pairs := memoryPair.FindAllStringSubmatch(cells[hbmColumn], -1)
		if len(pairs) == 0 {
			return nil, errors.New("hbm_unknown")
		}
		// npu-smi often puts memory and HBM in the same cell. HBM is last.
		pair := pairs[len(pairs)-1]
		used, e1 := strconv.ParseInt(pair[1], 10, 64)
		total, e2 := strconv.ParseInt(pair[2], 10, 64)
		if e1 != nil || e2 != nil || total <= 0 || used > total {
			return nil, errors.New("hbm_unknown")
		}
		key := current
		if len(ids) > 0 {
			if _, err := strconv.ParseUint(ids[0], 10, 64); err != nil {
				return nil, errors.New("npu_device_unknown")
			}
			key += "/" + ids[0]
		}
		if seen[key] {
			return nil, errors.New("npu_device_duplicate")
		}
		seen[key], cards[current] = true, true
		devices = append(devices, npuDevice{key, model, util, total - used})
	}
	if processRow {
		return nil, errors.New("npu_process_busy")
	}
	if !processTable || !noProcesses {
		return nil, errors.New("npu_process_unknown")
	}
	reading, err := parseNPUUtilization(output)
	if err != nil {
		return nil, errors.New("npu_devices_incomplete")
	}
	if !reading.occupancyKnown {
		return nil, errors.New("npu_process_unknown")
	}
	if reading.occupied {
		return nil, errors.New("npu_process_busy")
	}
	if len(devices) == 0 {
		return nil, errors.New("npu_devices_unknown")
	}
	for _, complete := range cards {
		if !complete {
			return nil, errors.New("npu_devices_incomplete")
		}
	}
	return devices, nil
}

func resourceSample(machine Machine, state State, req ResourceRequirements, now time.Time) ([]npuDevice, time.Time, error) {
	for key, value := range req.Labels {
		if machine.Labels[key] != value {
			return nil, time.Time{}, fmt.Errorf("label_mismatch:%s", key)
		}
	}
	var sample *Result
	for i := range state.Results {
		if state.Results[i].CommandID == "npu" {
			if sample != nil {
				return nil, time.Time{}, errors.New("npu_sample_duplicate")
			}
			sample = &state.Results[i]
		}
	}
	if sample == nil || sample.ExitCode == nil || *sample.ExitCode != 0 || sample.Error != "" || sample.Warning != "" || sample.Truncated {
		return nil, time.Time{}, errors.New("npu_sample_unknown")
	}
	at := sample.EndedAt
	if at.IsZero() || at.After(now) || now.Sub(at) > time.Duration(req.MaxSampleAge)*time.Second {
		return nil, at, errors.New("npu_sample_stale")
	}
	devices, err := parseNPUResources(sample.Stdout)
	if err != nil {
		return nil, at, err
	}
	if len(devices) != req.DevicesPerMachine {
		return nil, at, errors.New("npu_device_count_mismatch")
	}
	for _, device := range devices {
		if device.Model != req.Model {
			return nil, at, errors.New("npu_model_mismatch")
		}
		if device.Util > req.MaxUtilization {
			return nil, at, errors.New("npu_utilization_busy")
		}
		if device.FreeHBMMB < req.MinFreeHBMMB {
			return nil, at, errors.New("hbm_insufficient")
		}
	}
	return devices, at, nil
}
