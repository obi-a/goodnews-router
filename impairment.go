package goodnews

import (
	"fmt"
	"math"
)

const MaxDelayedPackets = 256

// Impairments apply to egress IPv4 packets. Down also blocks Ethernet/ARP.
// Corruption flips one IPv4 checksum bit after forwarding has repaired it.
type Impairment struct {
	Down           bool    `json:"down"`
	DelayMS        int     `json:"delay_ms"`
	LossPercent    float64 `json:"loss_percent"`
	CorruptPercent float64 `json:"corrupt_percent"`
}

func (i Impairment) Validate() error {
	if i.DelayMS < 0 || i.DelayMS > 10000 {
		return fmt.Errorf("delay_ms must be 0..10000")
	}
	for _, percent := range []float64{i.LossPercent, i.CorruptPercent} {
		if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
			return fmt.Errorf("loss_percent and corrupt_percent must be 0..100")
		}
	}
	return nil
}

type impairmentCommand struct {
	Interface string  `json:"interface,omitempty"`
	Setting   string  `json:"setting"`
	Value     float64 `json:"value,omitempty"`
}

func (r *Runtime) Impairments() map[string]Impairment {
	r.impairmentMu.RLock()
	defer r.impairmentMu.RUnlock()
	result := make(map[string]Impairment, len(r.impairments))
	for name, value := range r.impairments {
		result[name] = value
	}
	return result
}

func (r *Runtime) changeImpairment(command impairmentCommand) error {
	r.impairmentMu.Lock()
	defer r.impairmentMu.Unlock()
	if command.Setting == "clear" {
		if command.Interface != "" || command.Value != 0 {
			return fmt.Errorf("clear takes no interface or value")
		}
		for name := range r.impairments {
			r.impairments[name] = Impairment{}
		}
		return nil
	}
	i, ok := r.impairments[command.Interface]
	if !ok {
		return fmt.Errorf("unknown interface %q", command.Interface)
	}
	switch command.Setting {
	case "down", "up":
		if command.Value != 0 {
			return fmt.Errorf("up/down take no value")
		}
		i.Down = command.Setting == "down"
	case "delay":
		if math.IsNaN(command.Value) || math.IsInf(command.Value, 0) || command.Value < 0 || command.Value > 10000 || math.Trunc(command.Value) != command.Value {
			return fmt.Errorf("delay must be integer milliseconds in 0..10000")
		}
		i.DelayMS = int(command.Value)
	case "loss":
		i.LossPercent = command.Value
	case "corruption":
		i.CorruptPercent = command.Value
	default:
		return fmt.Errorf("setting must be up, down, delay, loss, corruption or clear; routing cost is a Stage 4 input")
	}
	if err := i.Validate(); err != nil {
		return err
	}
	r.impairments[command.Interface] = i
	return nil
}
