package focusrite2

import "testing"

// A successful pairing brings the client key and the trusted server; the
// mappings saved meanwhile stay.
func TestAdoptPairingKeepsMappings(t *testing.T) {
	saved := Config{DeviceSerial: "P9EAC6K250F325", MasterDim: ControlRef{Number: 4117, Path: "MainOutputs / Dim"},
		Host: "127.0.0.1", OnboardingPort: 1, SecurePort: 2, ServerPublicKey: "old", PrivateKey: "key"}
	saved.Outputs[0] = OutputBinding{Gain: ControlRef{Number: 4435}, Mute: ControlRef{Number: 4436}}
	paired := Config{Host: "127.0.0.1", OnboardingPort: 58323, SecurePort: 58322, ServerPublicKey: "new", PrivateKey: "key2"}
	paired.Outputs[0] = OutputBinding{Gain: ControlRef{Number: 1}} // stale copy from when pairing began

	got := saved.AdoptPairing(paired)
	if got.ServerPublicKey != "new" || got.PrivateKey != "key2" || got.OnboardingPort != 58323 || got.SecurePort != 58322 {
		t.Errorf("identity not adopted: %+v", got)
	}
	if got.Outputs != saved.Outputs || got.MasterDim != saved.MasterDim || got.DeviceSerial != saved.DeviceSerial {
		t.Error("mappings must stay as saved")
	}
}
