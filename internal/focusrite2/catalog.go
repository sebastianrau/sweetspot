package focusrite2

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Control is a discovered, standard AES70 object. Unknown vendor classes are
// retained for diagnostics but cannot be selected as writable controls.
type Control struct {
	Number uint32
	Parent uint32
	Role   string
	Path   string
	Class  string
	Kind   string
}

func (c Control) Ref() ControlRef { return ControlRef{Number: c.Number, Path: c.Path} }

type DeviceInfo struct {
	Model    string
	Serial   string
	Controls []Control
	Error    string
}

type member struct {
	number, parent uint32
	class          []uint16
}

func members(b []byte) ([]member, error) {
	return decodeMembers(b, 0, true)
}

func decodeMembers(b []byte, parent uint32, recursive bool) ([]member, error) {
	if len(b) < 2 {
		return nil, errors.New("truncated OCA member list")
	}
	n := int(be.Uint16(b))
	b = b[2:]
	var out []member
	for range n {
		if len(b) < 6 {
			return nil, errors.New("truncated OCA member")
		}
		m := member{number: be.Uint32(b), parent: parent}
		count := int(be.Uint16(b[4:]))
		b = b[6:]
		tail := 2 // class version
		if recursive {
			tail += 4 // containing block
		}
		if count > 32 || len(b) < count*2+tail {
			return nil, errors.New("invalid OCA class identification")
		}
		for range count {
			m.class = append(m.class, be.Uint16(b))
			b = b[2:]
		}
		if recursive {
			m.parent = be.Uint32(b[2:])
		}
		b = b[tail:]
		out = append(out, m)
	}
	if len(b) != 0 {
		return nil, errors.New("unexpected OCA member list suffix")
	}
	return out, nil
}
func classKind(class []uint16) string {
	for _, entry := range []struct {
		path []uint16
		kind string
	}{
		{[]uint16{1, 1, 1, 5}, "gain"}, {[]uint16{1, 1, 1, 2}, "mute"}, {[]uint16{1, 1, 1, 1, 1}, "bool"}, {[]uint16{1, 1, 2, 2}, "level"}, {[]uint16{1, 1, 2, 1, 12}, "string"}, {[]uint16{1, 1, 1, 1, 12}, "text"},
	} {
		if len(class) >= len(entry.path) && slices.Equal(class[:len(entry.path)], entry.path) {
			return entry.kind
		}
	}
	return ""
}

// Some FC2 versions do not implement GetMembersRecursive. Walk their
// nonrecursive GetMembers lists instead, preserving each object's parent.
func (c *oca) catalogMembers(ctx context.Context) ([]member, error) {
	b, err := c.call(ctx, 100, 3, 6, 0, nil)
	if err == nil {
		return members(b)
	}
	var status *ocaStatusError
	if !errors.As(err, &status) || status.status != 8 {
		return nil, err
	}
	queue := []uint32{100}
	seen := map[uint32]bool{100: true}
	var result []member
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		b, err = c.call(ctx, parent, 3, 5, 0, nil)
		if err != nil {
			return nil, err
		}
		children, err := decodeMembers(b, parent, false)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if seen[child.number] {
				return nil, errors.New("duplicate or cyclic OCA object number")
			}
			seen[child.number] = true
			result = append(result, child)
			if len(result) > 65535 {
				return nil, errors.New("OCA catalog limit exceeded")
			}
			if len(child.class) >= 3 && slices.Equal(child.class[:3], []uint16{1, 1, 3}) {
				queue = append(queue, child.number)
			}
		}
	}
	return result, nil
}

func (c *oca) catalog(ctx context.Context) (DeviceInfo, error) {
	info := DeviceInfo{Model: "Focusrite Control 2"}
	list, e := c.catalogMembers(ctx)
	if e != nil {
		return info, e
	}
	var b []byte
	byNumber := make(map[uint32]Control)
	for _, m := range list {
		if _, exists := byNumber[m.number]; exists {
			return info, errors.New("duplicate OCA object number")
		}
		b, e = c.call(ctx, m.number, 1, 5, 0, nil)
		if e != nil {
			return info, e
		}
		role, e := text(b)
		if e != nil {
			return info, e
		}
		parts := make([]string, len(m.class))
		for i, v := range m.class {
			parts[i] = strconv.Itoa(int(v))
		}
		byNumber[m.number] = Control{Number: m.number, Parent: m.parent, Role: role, Class: strings.Join(parts, "."), Kind: classKind(m.class)}
	}
	var path func(uint32, map[uint32]bool) (string, error)
	path = func(number uint32, seen map[uint32]bool) (string, error) {
		if number == 0 || number == 100 {
			return "", nil
		}
		control, ok := byNumber[number]
		if !ok {
			return fmt.Sprintf("#%d", number), nil
		}
		if seen[number] {
			return "", errors.New("cyclic OCA object hierarchy")
		}
		seen[number] = true
		parent, e := path(control.Parent, seen)
		delete(seen, number)
		if e != nil {
			return "", e
		}
		name := control.Role
		if name == "" {
			name = fmt.Sprintf("#%d", number)
		}
		if parent != "" {
			name = parent + " / " + name
		}
		return name, nil
	}
	for _, control := range byNumber {
		control.Path, e = path(control.Number, map[uint32]bool{})
		if e != nil {
			return info, e
		}
		info.Controls = append(info.Controls, control)
		if control.Kind == "string" {
			normalized := strings.ToLower(strings.NewReplacer(" ", "", "_", "", "-", "").Replace(control.Role))
			if normalized == "productname" || normalized == "model" || normalized == "serialnumber" {
				b, e = c.call(ctx, control.Number, 5, 1, 0, nil)
				if e != nil {
					return info, e
				}
				value, e := text(b)
				if e != nil {
					return info, e
				}
				if normalized == "serialnumber" {
					info.Serial = value
				} else {
					info.Model = value
				}
			}
		}
	}
	sort.Slice(info.Controls, func(i, j int) bool {
		a, b := info.Controls[i], info.Controls[j]
		if a.Path == b.Path {
			return a.Number < b.Number
		}
		return a.Path < b.Path
	})
	return info, nil
}

func (info DeviceInfo) resolve(ref ControlRef, kinds ...string) (Control, error) {
	if ref.Number == 0 {
		return Control{}, nil
	}
	for _, c := range info.Controls {
		if c.Number == ref.Number && c.Path == ref.Path && slices.Contains(kinds, c.Kind) {
			return c, nil
		}
	}
	return Control{}, fmt.Errorf("FC2 mapping %q (#%d) changed or is unavailable; select it again in Configuration", ref.Path, ref.Number)
}

// MasterControl reports whether c can be the monitor Mute or Dim: an on/off
// control of the MainOutputs block.
func MasterControl(c Control) bool {
	return strings.HasPrefix(c.Path, "MainOutputs / ") && (c.Kind == "mute" || c.Kind == "bool")
}

// nameControls returns the name controls next to an output's gain: the
// user-set CustomName and the fixed Name as fallback. Either may be missing.
func (info DeviceInfo) nameControls(o OutputBinding) (custom, fixed Control) {
	gain, e := info.resolve(o.Gain, "gain")
	if e != nil || gain.Number == 0 {
		return Control{}, Control{}
	}
	for _, c := range info.Controls {
		if c.Parent != gain.Parent {
			continue
		}
		switch {
		case c.Role == "CustomName" && c.Kind == "text":
			custom = c
		case c.Role == "Name" && c.Kind == "string":
			fixed = c
		}
	}
	return custom, fixed
}

func (info DeviceInfo) ValidateMappings(cfg Config) error {
	if cfg.DeviceSerial != "" && cfg.DeviceSerial != info.Serial {
		return fmt.Errorf("FC2 has a different interface; expected %s, found %s", cfg.DeviceSerial, info.Serial)
	}
	for _, ref := range []ControlRef{cfg.MasterMute, cfg.MasterDim} {
		c, e := info.resolve(ref, "bool", "mute")
		if e != nil {
			return e
		}
		if ref.Number != 0 && !MasterControl(c) {
			return fmt.Errorf("FC2 monitor Mute/Dim %q is not a MainOutputs control; select it again in Configuration", ref.Path)
		}
	}
	used := make(map[uint32]bool)
	for _, o := range cfg.Outputs {
		if (o.Gain.Number == 0) != (o.Mute.Number == 0) || (o.GainRight.Number == 0) != (o.MuteRight.Number == 0) || (o.GainRight.Number != 0 && o.Gain.Number == 0) {
			return errors.New("each FC2 speaker output needs both a gain and a mute control")
		}
		for _, v := range []struct {
			r ControlRef
			k string
		}{{o.Gain, "gain"}, {o.Mute, "mute"}, {o.GainRight, "gain"}, {o.MuteRight, "mute"}, {o.MeterLeft, "level"}, {o.MeterRight, "level"}} {
			kinds := []string{v.k}
			if v.k == "mute" {
				kinds = append(kinds, "bool")
			}
			if _, e := info.resolve(v.r, kinds...); e != nil {
				return e
			}
			if v.r.Number != 0 && (v.k == "gain" || v.k == "mute") {
				if used[v.r.Number] {
					return fmt.Errorf("FC2 output control %q is assigned more than once", v.r.Path)
				}
				used[v.r.Number] = true
			}
		}
	}
	if used[cfg.MasterMute.Number] || used[cfg.MasterDim.Number] || (cfg.MasterMute.Number != 0 && cfg.MasterMute.Number == cfg.MasterDim.Number) {
		return errors.New("FC2 master and output mappings must use different controls")
	}
	return nil
}
