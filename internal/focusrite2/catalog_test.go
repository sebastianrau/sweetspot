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
