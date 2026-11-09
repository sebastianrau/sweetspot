package focusrite2

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/sebastianrau/sweetspot/internal/domain"
)

func testOCA(t *testing.T, respond func(uint32, uint16, uint16, []byte) ([]byte, error)) *oca {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, e := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer ws.Close()
		for {
			_, b, e := ws.ReadMessage()
			if e != nil {
				return
			}
			if len(b) < 27 || b[7] != 1 {
				continue
			}
			body, e := respond(be.Uint32(b[18:]), be.Uint16(b[22:]), be.Uint16(b[24:]), b[27:])
			status := byte(0)
			if e != nil {
				status = 8
			}
			response := make([]byte, 10)
			be.PutUint32(response, uint32(10+len(body)))
			copy(response[4:8], b[14:18])
			response[8] = status
			if len(body) > 0 {
				response[9] = 1
			}
			response = append(response, body...)
			if e = ws.WriteMessage(websocket.BinaryMessage, frame(3, response)); e != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, e := dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { c.ws.Close() })
	return c
}
func ocaText(s string) []byte {
	p := make([]byte, 2)
	be.PutUint16(p, uint16(utf8.RuneCountInString(s)))
	return append(p, []byte(s)...)
}
func memberBytes(number, parent uint32, class []uint16) []byte {
	b := make([]byte, 6)
	be.PutUint32(b, number)
	be.PutUint16(b[4:], uint16(len(class)))
	for _, v := range class {
		p := make([]byte, 2)
		be.PutUint16(p, v)
		b = append(b, p...)
	}
	p := make([]byte, 6)
	be.PutUint16(p, 1)
	be.PutUint32(p[2:], parent)
	return append(b, p...)
}
func TestCatalogDoesNotDependOnModelOrObjectNumbers(t *testing.T) {
	for _, fixture := range []struct {
		model  string
		offset uint32
	}{{"Scarlett 4i4 (3rd Gen)", 70000}, {"Another FC2 interface", 90000}} {
		t.Run(fixture.model, func(t *testing.T) {
			offset := fixture.offset
			roles := map[uint32]string{offset: "Outputs", offset + 1: "Line 1–2 音量", offset + 2: "Mute", offset + 3: "ProductName", offset + 4: "SerialNumber", offset + 5: "Vendor function"}
			classes := [][]uint16{{1, 1, 3}, {1, 1, 1, 5}, {1, 1, 1, 2}, {1, 1, 2, 1, 12}, {1, 1, 2, 1, 12}, {1, 2, 65535, 0, 4878, 77}}
			c := testOCA(t, func(number uint32, level, index uint16, args []byte) ([]byte, error) {
				if len(args) != 0 {
					return nil, fmt.Errorf("unexpected arguments")
				}
				if number == 100 && level == 3 && index == 6 {
					b := []byte{0, 6}
					for i, class := range classes {
						parent := uint32(100)
						if i == 1 || i == 2 {
							parent = offset
						}
						b = append(b, memberBytes(offset+uint32(i), parent, class)...)
					}
					return b, nil
				}
				if level == 1 && index == 5 {
					return ocaText(roles[number]), nil
				}
				if level == 5 && index == 1 {
					if number == offset+3 {
						return ocaText(fixture.model), nil
					}
					if number == offset+4 {
						return ocaText("SERIAL-123"), nil
					}
				}
				return nil, fmt.Errorf("unexpected call")
			})
			info, e := c.catalog(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if info.Model != fixture.model || info.Serial != "SERIAL-123" || len(info.Controls) != 6 {
				t.Fatalf("bad catalog: %+v", info)
			}
			var cfg Config
			for _, control := range info.Controls {
				switch control.Kind {
				case "gain":
					cfg.Outputs[0].Gain = control.Ref()
				case "mute":
					cfg.Outputs[0].Mute = control.Ref()
				}
			}
			if e = info.ValidateMappings(cfg); e != nil {
				t.Fatal(e)
			}
			cfg.Outputs[0].Gain.Path = "different output"
			if e = info.ValidateMappings(cfg); e == nil {
				t.Fatal("changed object layout accepted")
			}
		})
	}
}

func TestOutputPushOrdersMuteBeforeGainAndAppliesSoftwareDim(t *testing.T) {
	type call struct {
		number uint32
		value  byte
		gain   float32
	}
	calls := make(chan call, 10)
	c := testOCA(t, func(number uint32, level, index uint16, args []byte) ([]byte, error) {
		if level != 4 || index != 2 {
			return nil, fmt.Errorf("unexpected setter")
		}
		v := call{number: number}
		if len(args) == 1 {
			v.value = args[0]
		} else if len(args) == 4 {
			v.gain = math.Float32frombits(be.Uint32(args))
		} else {
			return nil, fmt.Errorf("wrong setter payload")
		}
		calls <- v
		return nil, nil
	})
	cfg := Config{}
	cfg.Outputs[0] = OutputBinding{Gain: ControlRef{Number: 80000}, Mute: ControlRef{Number: 80001}}
	cfg.Outputs[1] = OutputBinding{Gain: ControlRef{Number: 90000}, Mute: ControlRef{Number: 90001}}
	controls := map[uint32]Control{80000: {Number: 80000, Kind: "gain"}, 80001: {Number: 80001, Kind: "mute"}, 90000: {Number: 90000, Kind: "gain"}, 90001: {Number: 90001, Kind: "mute"}}
	known := map[uint32]value{80000: {number: -10, min: -80, max: 6}, 80001: {on: true}, 90000: {number: -10, min: -80, max: 6}, 90001: {on: false}}
	s := domain.DefaultState()
	s.Master.Mute = false
	s.Master.VolumeDB = -25
	s.Master.Dim = true
	s.Master.DimDB = 20
	s.Speakers[domain.Sub].Selected = false
	if e := push(context.Background(), c, cfg, controls, known, s); e != nil {
		t.Fatal(e)
	}
	got := []call{}
	for len(calls) > 0 {
		got = append(got, <-calls)
	}
	if len(got) != 4 || got[0].number != 90001 || got[0].value != 1 || got[1].gain != -45 || got[2].gain != -45 || got[3].number != 80001 || got[3].value != 2 {
		t.Fatalf("wrong output changes/order: %+v", got)
	}
	if e := push(context.Background(), c, cfg, controls, known, s); e != nil {
		t.Fatal(e)
	}
	if len(calls) != 0 {
		t.Fatal("unchanged state generated writes")
	}
	if e := push(context.Background(), c, Config{}, nil, nil, s); e != nil {
		t.Fatal(e)
	}
	if len(calls) != 0 {
		t.Fatal("unmapped state generated writes")
	}
}

func TestCatalogWalksBlocksWhenRecursiveMethodUnsupported(t *testing.T) {
	c := testOCA(t, func(number uint32, level, index uint16, args []byte) ([]byte, error) {
		if level != 3 || len(args) != 0 {
			return nil, fmt.Errorf("unexpected request")
		}
		if index == 6 {
			return nil, fmt.Errorf("not implemented")
		}
		if index != 5 {
			return nil, fmt.Errorf("unexpected method")
		}
		var child []byte
		switch number {
		case 100:
			child = memberBytes(70000, 100, []uint16{1, 1, 3})
		case 70000:
			child = memberBytes(70001, 70000, []uint16{1, 1, 1, 5})
		default:
			return nil, fmt.Errorf("unexpected block %d", number)
		}
		// GetMembers returns object identification, without a parent field.
		return append([]byte{0, 1}, child[:len(child)-4]...), nil
	})
	list, err := c.catalogMembers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].number != 70000 || list[0].parent != 100 || list[1].number != 70001 || list[1].parent != 70000 {
		t.Fatalf("incorrect hierarchy: %+v", list)
	}
}

// Monitor Mute and Dim come from MainOutputs only; speaker names from the
// CustomName (string actuator) or Name next to the assigned gain.
func TestMasterControlsAndNameControls(t *testing.T) {
	if classKind([]uint16{1, 1, 1, 1, 12}) != "text" || classKind([]uint16{1, 1, 1, 1, 1}) != "bool" {
		t.Fatal("string actuator must be a text control")
	}
	info := DeviceInfo{Controls: []Control{
		{Number: 4116, Parent: 4112, Role: "Mute", Path: "MainOutputs / Mute", Kind: "mute"},
		{Number: 4117, Parent: 4112, Role: "Dim", Path: "MainOutputs / Dim", Kind: "bool"},
		{Number: 4114, Parent: 4112, Role: "LeftMeter", Path: "MainOutputs / LeftMeter", Kind: "level"},
		{Number: 4127, Parent: 4118, Role: "Pad", Path: "Inputs / 1 / Pad", Kind: "bool"},
		{Number: 4436, Parent: 4430, Role: "Mute", Path: "Mixes / 0 / Mute", Kind: "mute"},
		{Number: 4435, Parent: 4430, Role: "Gain", Path: "Mixes / 0 / Gain", Kind: "gain"},
		{Number: 4432, Parent: 4430, Role: "CustomName", Path: "Mixes / 0 / CustomName", Kind: "text"},
		{Number: 4431, Parent: 4430, Role: "Name", Path: "Mixes / 0 / Name", Kind: "string"},
		{Number: 4479, Parent: 4478, Role: "Name", Path: "Mixes / 0 / Channels / 0 / Name", Kind: "string"},
	}}
	var master []uint32
	for _, c := range info.Controls {
		if MasterControl(c) {
			master = append(master, c.Number)
		}
	}
	if fmt.Sprint(master) != "[4116 4117]" {
		t.Errorf("master controls = %v, want MainOutputs Mute and Dim only", master)
	}
	custom, fixed := info.nameControls(OutputBinding{Gain: ControlRef{Number: 4435, Path: "Mixes / 0 / Gain"}})
	if custom.Number != 4432 || fixed.Number != 4431 {
		t.Errorf("name controls = %d/%d, want 4432/4431", custom.Number, fixed.Number)
	}
	if custom, fixed = info.nameControls(OutputBinding{}); custom.Number != 0 || fixed.Number != 0 {
		t.Error("an unassigned output has no name controls")
	}
}

func TestReadTextControl(t *testing.T) {
	c := testOCA(t, func(number uint32, level, index uint16, _ []byte) ([]byte, error) {
		if number != 4432 || level != 5 || index != 1 {
			return nil, fmt.Errorf("unexpected getter %d %d.%d", number, level, index)
		}
		return ocaText("KRK VXT6"), nil
	})
	v, e := c.read(context.Background(), Control{Number: 4432, Kind: "text"})
	if e != nil || v.text != "KRK VXT6" {
		t.Fatalf("text = %q, %v", v.text, e)
	}
}

// With FC2 Dim mapped, the outputs still carry the dim attenuation (FC2's
// MainOutputs Dim does not reach mixes) and FC2 Dim follows.
func TestDimLowersOutputsAndSyncsFC2Dim(t *testing.T) {
	type call struct {
		number uint32
		level  uint16
		args   []byte
	}
	calls := make(chan call, 10)
	c := testOCA(t, func(number uint32, level, index uint16, args []byte) ([]byte, error) {
		if index != 2 {
			return nil, fmt.Errorf("unexpected method")
		}
		calls <- call{number, level, append([]byte(nil), args...)}
		return nil, nil
	})
	cfg := Config{MasterDim: ControlRef{Number: 4117}}
	cfg.Outputs[0] = OutputBinding{Gain: ControlRef{Number: 4435}, Mute: ControlRef{Number: 4436}}
	controls := map[uint32]Control{4117: {Number: 4117, Kind: "bool"}, 4435: {Number: 4435, Kind: "gain"}, 4436: {Number: 4436, Kind: "mute"}}
	known := map[uint32]value{4117: {on: false}, 4435: {number: -25, min: -127, max: 0}, 4436: {on: false}}
	s := domain.DefaultState()
	s.Master.Mute, s.Master.VolumeDB, s.Master.Dim, s.Master.DimDB = false, -25, true, 20
	s.Speakers[0].Selected = true
	if e := push(context.Background(), c, cfg, controls, known, s); e != nil {
		t.Fatal(e)
	}
	var dim, gain bool
	for len(calls) > 0 {
		v := <-calls
		switch {
		case v.number == 4117 && v.level == 5 && len(v.args) == 1 && v.args[0] == 1:
			dim = true
		case v.number == 4435 && len(v.args) == 4 && math.Float32frombits(be.Uint32(v.args)) == -45:
			gain = true
		}
	}
	if !dim || !gain {
		t.Errorf("FC2 Dim on: %t, gain -45 dB: %t", dim, gain)
	}
}

// Connecting never makes a later un-dim or un-mute louder than before.
func TestConnectAdoptsVolumeSafely(t *testing.T) {
	cfg := Config{MasterDim: ControlRef{Number: 4117}}
	cfg.Outputs[domain.SpeakerA] = OutputBinding{Gain: ControlRef{Number: 4435}, Mute: ControlRef{Number: 4436}}
	saved := func(volume int, dim bool) domain.State {
		s := domain.DefaultState()
		s.Master.Mute, s.Master.VolumeDB, s.Master.Dim, s.Master.DimDB = false, volume, dim, 20
		s.Speakers[domain.SpeakerA].Selected = true
		return s
	}
	connect := func(s domain.State, gain, floor float32, fc2Dim bool) domain.State {
		known := map[uint32]value{4435: {number: gain, min: floor, max: 0}, 4117: {on: fc2Dim}}
		snap := domain.Snapshot{State: s}
		for _, cmd := range connectCommands(s, cfg, known) {
			snap = domain.Apply(snap, cmd)
		}
		return snap.State
	}
	for _, c := range []struct {
		name               string
		volume             int
		savedDim, fc2Dim   bool
		gain, floor        float32
		wantVolume, wantDB int // volume, and the gain after un-dimming
	}{
		{"FC2 dimmed while closed", -30, false, true, -30, -127, -30, -30},
		{"undimmed in FC2 while closed", -30, true, false, -50, -127, -30, -30},
		{"dimmed by Sweetspot", -30, true, true, -50, -127, -30, -30},
		{"dim on during update from 0.9.0", -30, true, true, -30, -127, -30, -30},
		{"gain at -127 with dim", -120, true, true, -127, -127, -120, -120},
		{"gain at the device minimum", -100, false, false, -70, -70, -100, -100},
		{"fader moved down in FC2", -30, false, false, -40, -127, -40, -40},
	} {
		s := connect(saved(c.volume, c.savedDim), c.gain, c.floor, c.fc2Dim)
		if s.Master.VolumeDB != c.wantVolume || s.Master.Dim != c.fc2Dim {
			t.Errorf("%s: volume %d dim %t, want %d %t", c.name, s.Master.VolumeDB, s.Master.Dim, c.wantVolume, c.fc2Dim)
		}
		s.Master.Dim = false
		if got := s.OutputGainDB(); got != c.wantDB {
			t.Errorf("%s: %d dB after un-dim, want %d", c.name, got, c.wantDB)
		}
	}
	// While connected, a fader raised in FC2 is followed, also when dimmed.
	if got := adoptVolume(saved(-30, true), value{number: -20, min: -127}, false); got != 0 {
		t.Errorf("live adoption while dimmed = %d, want 0", got)
	}
}

func TestMappedSource(t *testing.T) {
	var cfg Config
	cfg.Outputs[domain.SpeakerB].Gain = ControlRef{Number: 1}
	cfg.Outputs[domain.Sub].Gain = ControlRef{Number: 2}
	s := domain.DefaultState()
	s.Speakers[domain.SpeakerA].Selected = true
	s.Speakers[domain.SpeakerB].Selected = true
	if id, ok := mappedSource(s, cfg); !ok || id != domain.SpeakerB {
		t.Errorf("source = %v %t, want the mapped Speaker B", id, ok)
	}
	s.Speakers[domain.SpeakerB].Selected = false
	s.Speakers[domain.Sub].Selected = true
	if id, ok := mappedSource(s, cfg); !ok || id != domain.Sub {
		t.Errorf("source = %v %t, want the mapped sub", id, ok)
	}
	s.Speakers[domain.Sub].Selected = false
	if _, ok := mappedSource(s, cfg); ok {
		t.Error("no source without a selected, mapped speaker")
	}
}

func TestMasterMappingMustBeMainOutputs(t *testing.T) {
	info := DeviceInfo{Controls: []Control{
		{Number: 4117, Parent: 4112, Role: "Dim", Path: "MainOutputs / Dim", Kind: "bool"},
		{Number: 4452, Parent: 4446, Role: "Mute", Path: "Mixes / 2 / Mute", Kind: "mute"},
	}}
	if e := info.ValidateMappings(Config{MasterDim: ControlRef{Number: 4117, Path: "MainOutputs / Dim"}}); e != nil {
		t.Errorf("MainOutputs Dim rejected: %v", e)
	}
	if e := info.ValidateMappings(Config{MasterDim: ControlRef{Number: 4452, Path: "Mixes / 2 / Mute"}}); e == nil || !strings.Contains(e.Error(), "MainOutputs") {
		t.Errorf("a mix mute as Dim must be rejected, got %v", e)
	}
}

// FC2 meters a mix after its gain. Sweetspot reports the level before the
// fader, so PRE stays put when the volume moves and POST gives FC2's value.
func TestFC2MeterPreAndPost(t *testing.T) {
	out := OutputBinding{Gain: ControlRef{Number: 4435}, MeterLeft: ControlRef{Number: 4433}, MeterRight: ControlRef{Number: 4434}}
	actual := map[uint32]value{4435: {number: -48}, 4433: {number: -60}, 4434: {number: -127}}
	got := preFaderLevels(out, actual)
	if got.LeftDB != -12 || got.RightDB != domain.SilenceDB {
		t.Fatalf("pre levels = %+v, want -12 and silence", got)
	}

	snap := domain.Snapshot{State: domain.DefaultState(), Levels: got}
	snap.State.Master.Mute, snap.State.Master.VolumeDB = false, -48
	snap.State.Speakers[domain.SpeakerA].Selected = true
	if l := snap.MeterLevels(); l.LeftDB != -12 {
		t.Errorf("PRE shows %v, want -12", l.LeftDB)
	}
	snap.State.Master.MeterPost = true
	if l := snap.MeterLevels(); l.LeftDB != -60 || l.RightDB != domain.SilenceDB {
		t.Errorf("POST shows %+v, want FC2's -60 and silence", l)
	}

	// A separately assigned right gain recalculates the right meter.
	out.GainRight = ControlRef{Number: 4440}
	actual[4440] = value{number: -20}
	actual[4434] = value{number: -30}
	if got = preFaderLevels(out, actual); got.RightDB != -10 {
		t.Errorf("right pre level = %v, want -10", got.RightDB)
	}
}

// After standby the interface reconnects over USB and FC2 numbers all its
// objects anew. The mappings follow their unchanged paths.
func TestRebindFollowsRenumberedControls(t *testing.T) {
	saved := Config{
		MasterMute: ControlRef{Number: 4116, Path: "MainOutputs / Mute"},
		MasterDim:  ControlRef{Number: 4117, Path: "MainOutputs / Dim"},
	}
	saved.Outputs[0] = OutputBinding{
		Gain:      ControlRef{Number: 4451, Path: "Mixes / 2 / Gain"},
		Mute:      ControlRef{Number: 4452, Path: "Mixes / 2 / Mute"},
		MeterLeft: ControlRef{Number: 4449, Path: "Mixes / 2 / LeftMeter"},
	}
	info := DeviceInfo{Controls: []Control{
		{Number: 6082, Path: "MainOutputs / Mute", Kind: "mute"},
		{Number: 6083, Path: "MainOutputs / Dim", Kind: "bool"},
		{Number: 6417, Path: "Mixes / 2 / Gain", Kind: "gain"},
		{Number: 6418, Path: "Mixes / 2 / Mute", Kind: "mute"},
		{Number: 6415, Path: "Mixes / 2 / LeftMeter", Kind: "level"},
		// The old number of Mixes / 2 / Gain now belongs to another control.
		{Number: 4451, Path: "Inputs / 7 / Gain", Kind: "gain"},
	}}
	if e := info.ValidateMappings(saved); e == nil {
		t.Fatal("the saved numbers must not validate as they are")
	}
	got := info.Rebind(saved)
	if e := info.ValidateMappings(got); e != nil {
		t.Fatalf("rebound mappings rejected: %v", e)
	}
	want := map[string]uint32{"mute": 6082, "dim": 6083, "gain": 6417, "outmute": 6418, "meter": 6415}
	have := map[string]uint32{"mute": got.MasterMute.Number, "dim": got.MasterDim.Number, "gain": got.Outputs[0].Gain.Number, "outmute": got.Outputs[0].Mute.Number, "meter": got.Outputs[0].MeterLeft.Number}
	for k, n := range want {
		if have[k] != n {
			t.Errorf("%s = #%d, want #%d", k, have[k], n)
		}
	}
	if got.Outputs[0].Gain.Path != "Mixes / 2 / Gain" || got.Outputs[1] != (OutputBinding{}) {
		t.Errorf("paths or unmapped outputs changed: %+v", got.Outputs[:2])
	}
	if again := info.Rebind(got); again != got {
		t.Error("a current mapping must stay as it is")
	}

	// A path that is gone stays unavailable; it is not guessed.
	gone := Config{MasterDim: ControlRef{Number: 4117, Path: "MainOutputs / Dim"}}
	if got := (DeviceInfo{}).Rebind(gone); got != gone {
		t.Errorf("missing path rebound to %+v", got.MasterDim)
	}
}
