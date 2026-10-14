package watch

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Only the machine mean survives parsing. Device rows never enter the store.
type machineReading struct {
	basis          int64 // percent, multiplied by 100
	model          string
	metric         string
	occupancyKnown bool
	occupied       bool
}

// Read only the process-table PID column. A positive PID on any device means
// the whole machine is occupied; absent/ambiguous tables never mean idle.
func parseNPUOccupancy(output string, cards map[string]bool) (occupied, known bool) {
	pidColumn := -1
	noProcesses, malformed := false, false
	idleCards := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		cells := tableCells(line)
		for i, cell := range cells {
			if strings.EqualFold(strings.TrimSpace(cell), "Process id") {
				pidColumn = i
			}
		}
		if pidColumn < 0 || len(cells) == 0 {
			continue
		}
		if len(cells) == 1 && (strings.EqualFold(cells[0], "No running processes found") || strings.EqualFold(cells[0], "No running process found")) {
			noProcesses = true
			continue
		}
		const idlePrefix = "no running processes found in npu "
		if len(cells) == 1 && strings.HasPrefix(strings.ToLower(cells[0]), idlePrefix) {
			id, err := strconv.ParseUint(strings.TrimSpace(cells[0][len(idlePrefix):]), 10, 64)
			if err != nil {
				malformed = true
			} else {
				idleCards[strconv.FormatUint(id, 10)] = true
			}
			continue
		}
		if pidColumn < len(cells) && strings.EqualFold(cells[pidColumn], "Process id") {
			continue
		}
		if pidColumn >= len(cells) {
			malformed = true
			continue
		}
		pid, err := strconv.ParseUint(cells[pidColumn], 10, 64)
		if err == nil && pid > 0 {
			return true, true
		}
		malformed = true
	}
	if !noProcesses && len(cards) > 0 {
		noProcesses = true
		for id := range cards {
			if !idleCards[id] {
				noProcesses = false
				break
			}
		}
	}
	return false, pidColumn >= 0 && noProcesses && !malformed
}

func tableCells(line string) []string {
	if !strings.HasPrefix(strings.TrimSpace(line), "|") {
		return nil
	}
	parts := strings.Split(strings.TrimSpace(line), "|")
	if len(parts) < 3 {
		return nil
	}
	cells := parts[1 : len(parts)-1]
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func parseNPUUtilization(output string) (machineReading, error) {
	var reading machineReading
	nameColumn, utilColumn := -1, -1
	current, model := "", ""
	models, devices, cards := map[string]bool{}, map[string]float64{}, map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		cells := tableCells(line)
		if len(cells) == 0 {
			continue
		}
		if strings.Contains(line, "Process id") || strings.Contains(line, "Process ID") {
			break
		}
		if strings.Contains(line, "NPU") && strings.Contains(line, "Health") {
			for i, cell := range cells {
				if strings.Contains(cell, "Name") {
					nameColumn = i
				}
			}
			continue
		}
		foundHeader := false
		for i, cell := range cells {
			for _, metric := range []string{"AICore(%)", "NPU Util(%)"} {
				if strings.Contains(cell, metric) {
					if reading.metric != "" && reading.metric != metric {
						return reading, fmt.Errorf("混合利用率表头")
					}
					reading.metric, utilColumn, foundHeader = metric, i, true
				}
			}
		}
		if foundHeader || nameColumn < 0 || utilColumn < 0 || nameColumn >= len(cells) || utilColumn >= len(cells) {
			continue
		}
		id := strings.Fields(cells[0])
		name := strings.Fields(cells[nameColumn])
		// A2/A3 use one cell for NPU ID + model, A5 uses separate cells.
		if nameColumn == 0 && len(name) > 1 {
			name = name[1:]
		}
		if len(id) > 0 && len(name) > 0 {
			_, idErr := strconv.Atoi(id[0])
			_, nameErr := strconv.Atoi(name[0])
			if idErr == nil && nameErr != nil {
				if name[0] == "NA" || name[0] == "N/A" {
					return reading, fmt.Errorf("NPU 型号缺失")
				}
				current, model = id[0], name[0]
				models[model], cards[current] = true, false
				continue
			}
		}
		if nameColumn > 0 && len(id) > 0 && len(name) == 0 {
			if _, err := strconv.Atoi(id[0]); err == nil {
				return reading, fmt.Errorf("NPU 型号缺失")
			}
		}
		if current == "" {
			continue
		}
		values := strings.Fields(cells[utilColumn])
		if len(values) == 0 {
			return reading, fmt.Errorf("利用率行缺失")
		}
		value, err := strconv.ParseFloat(strings.TrimSuffix(values[0], "%"), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return reading, fmt.Errorf("利用率数值无效")
		}
		key := current
		if len(id) > 0 {
			if _, err := strconv.Atoi(id[0]); err != nil {
				return reading, fmt.Errorf("芯片标识无效")
			}
			key += "/" + id[0]
		}
		if _, duplicate := devices[key]; duplicate {
			return reading, fmt.Errorf("重复利用率行")
		}
		devices[key], cards[current] = value, true
	}
	if len(devices) == 0 || reading.metric == "" {
		return reading, fmt.Errorf("没有识别到完整 NPU 利用率表")
	}
	for _, present := range cards {
		if !present {
			return reading, fmt.Errorf("部分 NPU 利用率缺失")
		}
	}
	var sum float64
	for _, value := range devices {
		sum += value
	}
	reading.basis = int64(math.Round(sum / float64(len(devices)) * 100))
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	// A mixed-model machine is deliberately rejected rather than combining metrics.
	if len(names) != 1 {
		return reading, fmt.Errorf("机器包含不同 NPU 型号")
	}
	reading.model = names[0]
	reading.occupied, reading.occupancyKnown = parseNPUOccupancy(output, cards)
	return reading, nil
}
