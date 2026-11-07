package focusrite2

import (
	"fmt"
	"testing"
)

func TestSpeakerTargets(t *testing.T) {
	for _, count := range []int{3, 6} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			info := DeviceInfo{Controls: []Control{{Number: 10, Parent: 100, Role: "Mixes"}, {Number: 11, Parent: 100, Role: "Outputs"}}}
			add := func(parent uint32, role string, base uint32) {
				info.Controls = append(info.Controls, Control{Number: base, Parent: parent, Role: role})
				for i, v := range []struct{ role, kind string }{{"Gain", "gain"}, {"Mute", "mute"}, {"LeftMeter", "level"}, {"RightMeter", "level"}} {
					info.Controls = append(info.Controls, Control{Number: base + uint32(i) + 1, Parent: base, Role: v.role, Kind: v.kind, Path: fmt.Sprintf("%d/%s", base, v.role)})
				}
			}
			for i := 0; i < count; i++ {
				add(10, fmt.Sprint(i), uint32(1000+i*100))
				add(uint32(1000+i*100), "Channels", uint32(5000+i*100))
			}
			add(11, "Monitors", 9000)
			// An output exposing meters alone cannot be used as a volume control.
			info.Controls = append(info.Controls, Control{Number: 9990, Parent: 11, Role: "1"}, Control{Number: 9991, Parent: 9990, Role: "Level", Kind: "level"})
			targets := info.SpeakerTargets()
			if len(targets) != count+1 {
				t.Fatalf("got %d targets, want %d", len(targets), count+1)
			}
			if targets[0].Label != "Output Monitors" {
				t.Fatal("direct output should be first")
			}
			for i, target := range targets[1:] {
				if target.Label != fmt.Sprintf("Mix %c", 'A'+i) {
					t.Fatalf("bad mix label: %s", target.Label)
				}
				base := uint32(1000 + i*100)
				if target.Binding.Gain.Number != base+1 || target.Binding.Mute.Number != base+2 || target.Binding.MeterLeft.Number != base+3 || target.Binding.MeterRight.Number != base+4 {
					t.Fatalf("incorrect bus controls: %+v", target)
				}
			}
		})
	}
}
