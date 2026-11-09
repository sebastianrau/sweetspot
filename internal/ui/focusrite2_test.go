package ui

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2/storage"
	"github.com/skip2/go-qrcode"

	"github.com/sebastianrau/sweetspot/internal/domain"
)

type submitter struct{ got []string }

func (s *submitter) Submit(v string) error { s.got = append(s.got, v); return nil }

// A screenshot file of FC2's QR is read and submitted to the pairing session.
func TestSubmitQRScreenshot(t *testing.T) {
	dir := t.TempDir()
	payload := strings.Repeat("a1b2c3d4", 16)
	code, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		t.Fatal(err)
	}
	shot := filepath.Join(dir, "Screenshot.png")
	f, err := os.Create(shot)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, code.Image(512)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var s submitter
	if err = submitQRScreenshot(&s, storage.NewFileURI(shot)); err != nil || len(s.got) != 1 || s.got[0] != payload {
		t.Fatalf("err=%v submitted=%d", err, len(s.got))
	}

	text := filepath.Join(dir, "notes.txt")
	if err = os.WriteFile(text, []byte("no image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = submitQRScreenshot(&s, storage.NewFileURI(text)); err == nil || len(s.got) != 1 {
		t.Errorf("a non-image file must be rejected without submitting (err=%v)", err)
	}
}

// Pairing runs as a modal dialog over the settings, so nothing else can be
// changed meanwhile; closing it ends the pairing.
func TestPairingIsModal(t *testing.T) {
	a, _ := newTestApp(t)
	s := a.settings
	s.live = domain.DefaultState()
	s.show()
	s.pairControl2()
	if s.pairDialog == nil || s.win.Canvas().Overlays().Top() == nil {
		t.Fatal("pairing must open a modal dialog over the settings")
	}
	s.pairControl2() // a second click shows the same dialog
	if n := len(s.win.Canvas().Overlays().List()); n != 1 {
		t.Errorf("%d overlays, want one pairing dialog", n)
	}
	s.pairDialog.Hide()
	if s.pairDialog != nil || s.win.Canvas().Overlays().Top() != nil {
		t.Error("closing the dialog must end the pairing")
	}
}
