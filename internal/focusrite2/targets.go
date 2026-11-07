package focusrite2

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SpeakerTarget groups the controls of a mix bus or direct output. Channel
// controls inside a mix are deliberately excluded.
type SpeakerTarget struct {
	Label   string
	Binding OutputBinding
}

func (info DeviceInfo) SpeakerTargets() []SpeakerTarget {
	byParent := make(map[uint32][]Control)
	for _, c := range info.Controls {
		byParent[c.Parent] = append(byParent[c.Parent], c)
	}
	var targets []SpeakerTarget
	for _, root := range info.Controls {
		if root.Parent != 100 || (root.Role != "Mixes" && root.Role != "Outputs") {
			continue
		}
		for _, group := range byParent[root.Number] {
			var binding OutputBinding
			ambiguous := false
			assign := func(ref *ControlRef, c Control) {
				if ref.Number != 0 {
					ambiguous = true
				}
				*ref = c.Ref()
			}
			for _, c := range byParent[group.Number] {
				switch {
				case c.Role == "Gain" && c.Kind == "gain":
					assign(&binding.Gain, c)
				case c.Role == "Mute" && (c.Kind == "mute" || c.Kind == "bool"):
					assign(&binding.Mute, c)
				case c.Role == "LeftMeter" && c.Kind == "level":
					assign(&binding.MeterLeft, c)
				case c.Role == "RightMeter" && c.Kind == "level":
					assign(&binding.MeterRight, c)
				}
			}
			if ambiguous || binding.Gain.Number == 0 || binding.Mute.Number == 0 {
				continue
			}
			label := "Output " + group.Role
			if root.Role == "Mixes" {
				label = "Mix " + group.Role
				if index, err := strconv.Atoi(group.Role); err == nil && index >= 0 && index < 26 {
					label = fmt.Sprintf("Mix %c", 'A'+index)
				}
			}
			targets = append(targets, SpeakerTarget{Label: label, Binding: binding})
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		a, b := targets[i].Label, targets[j].Label
		if strings.HasPrefix(a, "Output ") != strings.HasPrefix(b, "Output ") {
			return strings.HasPrefix(a, "Output ")
		}
		if a == b {
			return targets[i].Binding.Gain.Number < targets[j].Binding.Gain.Number
		}
		return a < b
	})
	return targets
}
