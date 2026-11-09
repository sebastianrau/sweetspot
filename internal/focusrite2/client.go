package focusrite2

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/sebastianrau/sweetspot/internal/domain"
	"github.com/sebastianrau/sweetspot/internal/logging"
)

var log = logging.New("focusrite2")

// nameInterval is how often the output and mix names are read again.
const nameInterval = 2 * time.Second

type value struct {
	number   float32
	on       bool
	min, max float32
	text     string
}

func (c *oca) read(ctx context.Context, control Control) (value, error) {
	var v value
	level := uint16(4)
	if control.Kind == "bool" || control.Kind == "text" || control.Kind == "string" {
		level = 5
	}
	b, e := c.call(ctx, control.Number, level, 1, 0, nil)
	if e != nil {
		return v, e
	}
	switch control.Kind {
	case "bool", "mute":
		if len(b) != 1 {
			return v, errors.New("invalid FC2 boolean response")
		}
		if control.Kind == "mute" {
			if b[0] != 1 && b[0] != 2 {
				return v, errors.New("invalid FC2 mute state")
			}
			v.on = b[0] == 1
		} else {
			if b[0] > 1 {
				return v, errors.New("invalid FC2 boolean state")
			}
			v.on = b[0] == 1
		}
	case "text", "string":
		if v.text, e = text(b); e != nil {
			return v, e
		}
	case "gain", "level":
		if len(b) != 12 {
			return v, errors.New("invalid FC2 level response")
		}
		v.number = math.Float32frombits(be.Uint32(b))
		v.min = math.Float32frombits(be.Uint32(b[4:]))
		v.max = math.Float32frombits(be.Uint32(b[8:]))
		for _, f := range []float32{v.number, v.min, v.max} {
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				return v, errors.New("non-finite FC2 level")
			}
		}
		if v.min > v.max {
			return v, errors.New("invalid FC2 level limits")
		}
	default:
		return v, fmt.Errorf("unsupported FC2 read class %s", control.Class)
	}
	return v, nil
}
func (c *oca) write(ctx context.Context, control Control, v value) error {
	var data []byte
	level := uint16(4)
	switch control.Kind {
	case "gain":
		data = make([]byte, 4)
		be.PutUint32(data, math.Float32bits(v.number))
	case "bool":
		level = 5
		data = []byte{0}
		if v.on {
			data[0] = 1
		}
	case "mute":
		data = []byte{2}
		if v.on {
			data[0] = 1
		}
	default:
		return fmt.Errorf("FC2 class %s is not writable", control.Class)
	}
	_, e := c.call(ctx, control.Number, level, 2, 1, data)
	return e
}

// Run discovers the live object model on each reconnect. Only explicit,
// type-checked mappings are written; there is no model or object-ID allowlist.
func Run(ctx context.Context, cfg Config, current func() domain.Snapshot, submit func(domain.Command), catalog func(DeviceInfo)) {
	var lastInfo DeviceInfo
	report := func(info DeviceInfo) {
		if info.Controls != nil {
			lastInfo = info
		} else {
			lastInfo.Error = info.Error
		}
		catalog(lastInfo)
	}
	for ctx.Err() == nil {
		live, e := Discover(ctx, cfg)
		// Discovery can be disabled by the network. Manual/stored endpoints still
		// work, but an observed identity change must never bypass key pinning.
		if e != nil && cfg.Validate() == nil && ctx.Err() == nil && !errors.Is(e, ErrIdentityChanged) {
			live = cfg
			e = nil
		}
		if e == nil {
			e = runSession(ctx, live, current, submit, report)
		}
		submit(domain.SetDevice{})
		if ctx.Err() != nil {
			return
		}
		report(DeviceInfo{Error: e.Error()})
		log.Warn("FC2 connection unavailable", "err", e)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
func runSession(ctx context.Context, cfg Config, current func() domain.Snapshot, submit func(domain.Command), catalog func(DeviceInfo)) error {
	timeout, cancel := context.WithTimeout(ctx, 5*time.Second)
	c, e := secure(timeout, cfg)
	cancel()
	if e != nil {
		return e
	}
	defer c.ws.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.ws.Close() })
	defer stop()
	request, done := context.WithTimeout(ctx, 30*time.Second)
	info, e := c.catalog(request)
	done()
	if e != nil {
		return e
	}
	catalog(info)
	if e = info.ValidateMappings(cfg); e != nil {
		return e
	}
	controls := make(map[uint32]Control)
	add := func(ref ControlRef, kinds ...string) {
		if ref.Number != 0 {
			control, _ := info.resolve(ref, kinds...)
			controls[ref.Number] = control
		}
	}
	add(cfg.MasterMute, "mute", "bool")
	add(cfg.MasterDim, "mute", "bool")
	device := domain.Device{Control2: true, ServerConnected: true, Present: true, Model: info.Model + " (FC2)", SerialNumber: info.Serial}
	for i, o := range cfg.Outputs {
		add(o.Gain, "gain")
		add(o.Mute, "mute", "bool")
		add(o.GainRight, "gain")
		add(o.MuteRight, "mute", "bool")
		add(o.MeterLeft, "level")
		add(o.MeterRight, "level")
		device.SpeakerOutputs[i] = o.Gain.Number != 0 && o.Mute.Number != 0
		device.HasVolume = device.HasVolume || o.Gain.Number != 0
		device.HasMute = device.HasMute || o.Mute.Number != 0
		device.HasMeter = device.HasMeter || (o.MeterLeft.Number != 0 && o.MeterRight.Number != 0)
	}
	device.HasMute = device.HasMute || cfg.MasterMute.Number != 0
	device.HasDim = device.HasVolume || cfg.MasterDim.Number != 0
	submit(domain.SetDevice{Device: device})
	read := func() (map[uint32]value, error) {
		result := make(map[uint32]value)
		for number, control := range controls {
			request, done := context.WithTimeout(ctx, 5*time.Second)
			v, e := c.read(request, control)
			done()
			if e != nil {
				return nil, e
			}
			result[number] = v
		}
		return result, nil
	}
	// Speakers take the name of their output or mix, as with FC1.
	type names struct{ custom, fixed Control }
	var speakerNames [domain.NumSpeakers]names
	for i, o := range cfg.Outputs {
		speakerNames[i].custom, speakerNames[i].fixed = info.nameControls(o)
	}
	snap := current()
	apply := func(cmd domain.Command) { submit(cmd); snap = domain.Apply(snap, cmd) }
	rename := func() error {
		for i, n := range speakerNames {
			name := ""
			for _, control := range []Control{n.custom, n.fixed} {
				if control.Number == 0 || name != "" {
					continue
				}
				request, done := context.WithTimeout(ctx, 5*time.Second)
				v, e := c.read(request, control)
				done()
				if e != nil {
					return e
				}
				name = strings.TrimSpace(v.text)
			}
			if name != "" && name != snap.State.Speakers[i].Name {
				apply(domain.RenameSpeaker{ID: domain.SpeakerID(i), Name: name})
			}
		}
		return nil
	}
	known, e := read()
	if e != nil {
		return e
	}
	if e = rename(); e != nil {
		return e
	}
	if cfg.MasterMute.Number != 0 {
		apply(domain.SetMute{On: known[cfg.MasterMute.Number].on})
	}
	if cfg.MasterDim.Number != 0 {
		apply(domain.SetDim{On: known[cfg.MasterDim.Number].on})
	}
	// Never raise a physical output on initial connection because of a stale
	// saved fader value. Adopt the selected output's current gain first.
	if source, ok := snap.State.MeterSource(); ok {
		if ref := cfg.Outputs[source].Gain; ref.Number != 0 {
			gain := known[ref.Number].number
			if snap.State.Master.Dim { // the output carries the dim attenuation
				gain += float32(snap.State.Master.DimDB)
			}
			apply(domain.SetVolume{DB: int(math.Round(float64(gain)))})
		}
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	// First push uses the locally adopted snapshot. Later iterations use the
	// controller snapshot after all initial commands have been consumed.
	lastKeepalive := time.Now()
	lastNames := time.Now()
	for {
		if time.Since(lastKeepalive) >= time.Second {
			if e = c.send(frame(4, []byte{0, 3})); e != nil {
				return e
			}
			lastKeepalive = time.Now()
		}
		if e = push(ctx, c, cfg, controls, known, snap.State); e != nil {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		snap = current()
		if time.Since(lastNames) >= nameInterval {
			if e = rename(); e != nil {
				return e
			}
			lastNames = time.Now()
		}
		actual, e := read()
		if e != nil {
			return e
		}
		if ref := cfg.MasterMute; ref.Number != 0 && actual[ref.Number].on != known[ref.Number].on {
			apply(domain.SetMute{On: actual[ref.Number].on})
		}
		if ref := cfg.MasterDim; ref.Number != 0 && actual[ref.Number].on != known[ref.Number].on {
			apply(domain.SetDim{On: actual[ref.Number].on})
		}
		levels := domain.Silence
		if source, ok := snap.State.MeterSource(); ok {
			out := cfg.Outputs[source]
			if ref := out.Gain; ref.Number != 0 && actual[ref.Number].number != known[ref.Number].number {
				gain := actual[ref.Number].number
				if snap.State.Master.Dim {
					gain += float32(snap.State.Master.DimDB)
				}
				apply(domain.SetVolume{DB: int(math.Round(float64(gain)))})
			}
			if out.MeterLeft.Number != 0 {
				levels.LeftDB = float64(actual[out.MeterLeft.Number].number)
			}
			if out.MeterRight.Number != 0 {
				levels.RightDB = float64(actual[out.MeterRight.Number].number)
			}
		}
		if snap.Levels != levels {
			apply(domain.SetLevels{Levels: levels})
		}
		known = actual
	}
}

func push(ctx context.Context, c *oca, cfg Config, controls map[uint32]Control, known map[uint32]value, s domain.State) error {
	type change struct {
		number uint32
		v      value
	}
	var mute, gain, unmute []change
	boolean := func(ref ControlRef, on bool) {
		if ref.Number == 0 || known[ref.Number].on == on {
			return
		}
		v := known[ref.Number]
		v.on = on
		if on {
			mute = append(mute, change{ref.Number, v})
		} else {
			unmute = append(unmute, change{ref.Number, v})
		}
	}
	boolean(cfg.MasterMute, s.Master.Mute)
	// As with FC1, Sweetspot dims the outputs itself; FC2's MainOutputs Dim
	// only follows, since it does not reach outputs fed by mixes.
	boolean(cfg.MasterDim, s.Master.Dim)
	db := s.OutputGainDB()
	for i, out := range cfg.Outputs {
		if s.Speakers[i].Disabled {
			continue
		}
		for _, pair := range []struct{ gain, mute ControlRef }{{out.Gain, out.Mute}, {out.GainRight, out.MuteRight}} {
			v := known[pair.gain.Number]
			// A gain below the device's supported range is represented by mute,
			// rather than playing louder than the requested monitor level.
			boolean(pair.mute, s.OutputMuted(domain.SpeakerID(i)) || (pair.gain.Number != 0 && float32(db) < v.min))
			if pair.gain.Number != 0 {
				target := min(v.max, max(v.min, float32(db)))
				if target != v.number {
					v.number = target
					gain = append(gain, change{pair.gain.Number, v})
				}
			}
		}
	}
	for _, changes := range [][]change{mute, gain, unmute} {
		for _, item := range changes {
			request, done := context.WithTimeout(ctx, 5*time.Second)
			e := c.write(request, controls[item.number], item.v)
			done()
			if e != nil {
				return e
			}
			known[item.number] = item.v
		}
	}
	return nil
}
