package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"
	"github.com/skip2/go-qrcode"

	"github.com/sebastianrau/sweetspot/internal/config"
	"github.com/sebastianrau/sweetspot/internal/domain"
	"github.com/sebastianrau/sweetspot/internal/focusrite2"
)

func (s *settingsWindow) control2Section() fyne.CanvasObject {
	hint := widget.NewLabel("Choose a direct output or a mix for each speaker. Gain, mute and meters are assigned together. If no direct output is available, choose a mix and route it to your speaker outputs in FC2.")
	hint.Wrapping = fyne.TextWrapWord
	state := "Not paired"
	if s.draft.Focusrite.Control2.PrivateKey != "" {
		state = "Client identity saved"
	}
	box := container.NewVBox(hint, widget.NewLabel(state), widget.NewButton("Pair with Focusrite Control 2…", s.pairControl2))
	var info *focusrite2.DeviceInfo
	for _, device := range s.devices {
		if device.Control2 != nil {
			info = device.Control2
			break
		}
	}
	if info == nil {
		box.Add(widget.NewLabel("Save to connect and discover FC2 controls."))
		return box
	}
	if info.Error != "" {
		label := widget.NewLabel(info.Error)
		label.Wrapping = fyne.TextWrapWord
		label.Importance = widget.WarningImportance
		box.Add(label)
	}
	if len(info.Controls) == 0 {
		return box
	}
	cfg := &s.draft.Focusrite.Control2
	binding := func(ref *focusrite2.ControlRef, kinds ...string) *widget.Select {
		options := []string{none}
		byLabel := map[string]focusrite2.ControlRef{}
		selected := none
		for _, control := range info.Controls {
			if !slices.Contains(kinds, control.Kind) {
				continue
			}
			label := control.Path + " (#" + strconv.FormatUint(uint64(control.Number), 10) + ")"
			options = append(options, label)
			byLabel[label] = control.Ref()
			if *ref == control.Ref() {
				selected = label
			}
		}
		if ref.Number != 0 && selected == none {
			selected = "Unavailable: " + ref.Path
			options = append(options, selected)
		}
		sel := newSelect(options, func(label string) { *ref = byLabel[label]; cfg.DeviceSerial = info.Serial })
		sel.Selected = selected
		return sel
	}
	form := container.New(layout.NewFormLayout(),
		widget.NewLabel("Interface:"), widget.NewLabel(info.Model+" · "+info.Serial),
		widget.NewLabel("Monitor Mute (optional):"), binding(&cfg.MasterMute, "mute", "bool"),
		widget.NewLabel("Native Dim (optional):"), binding(&cfg.MasterDim, "mute", "bool"),
	)
	targets := info.SpeakerTargets()
	for i := range cfg.Outputs {
		out := &cfg.Outputs[i]
		form.Add(heading(speakerLabel(domain.SpeakerID(i))))
		form.Add(layout.NewSpacer())
		name := widget.NewEntry()
		name.SetText(out.Name)
		name.SetPlaceHolder(speakerLabel(domain.SpeakerID(i)))
		name.OnChanged = func(v string) { out.Name = strings.TrimSpace(v) }
		form.Add(widget.NewLabel("Name:"))
		form.Add(name)
		options := []string{none}
		bindings := map[string]focusrite2.OutputBinding{none: {}}
		selected := none
		for _, target := range targets {
			label := target.Label
			options = append(options, label)
			bindings[label] = target.Binding
			current := *out
			current.Name = ""
			if current == target.Binding {
				selected = label
			}
		}
		if out.Gain.Number != 0 && selected == none {
			selected = "Existing assignment (select to replace)"
			options = append(options, selected)
			bindings[selected] = *out
		}
		selector := newSelect(options, func(label string) {
			binding, ok := bindings[label]
			if !ok {
				return
			}
			binding.Name = out.Name
			*out = binding
			cfg.DeviceSerial = info.Serial
		})
		selector.Selected = selected
		form.Add(widget.NewLabel("Output / mix:"))
		form.Add(selector)
	}
	box.Add(form)
	note := widget.NewLabel("Each speaker needs a different output or mix. Mix control affects every output fed by that mix; configure routing in FC2. Native Dim uses the amount set in FC2.")
	note.Wrapping = fyne.TextWrapWord
	box.Add(note)
	box.Add(widget.NewButton("Copy discovered controls", func() {
		var lines []string
		for _, control := range info.Controls {
			lines = append(lines, fmt.Sprintf("%d\t%s\t%s\t%s", control.Number, control.Class, control.Kind, control.Path))
		}
		fyne.CurrentApp().Clipboard().SetContent(strings.Join(lines, "\n"))
	}))
	return box
}

func (s *settingsWindow) pairControl2() {
	if s.pairCancel != nil {
		s.pairWindow.Show()
		s.pairWindow.RequestFocus()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	s.pairCancel = cancel
	win := fyne.CurrentApp().NewWindow("Sweetspot · FC2 Pairing")
	s.pairWindow = win
	status := widget.NewLabel("Finding Focusrite Control 2 on this computer…")
	status.Wrapping = fyne.TextWrapWord
	content := container.NewVBox(status)
	win.SetContent(container.NewVScroll(content))
	win.Resize(fyne.NewSize(420, 560))
	closePair := func() { cancel(); s.pairCancel = nil; s.pairWindow = nil; win.Close() }
	win.SetOnClosed(func() { cancel(); s.pairCancel = nil; s.pairWindow = nil })
	win.Show()
	go func() {
		cfg, e := focusrite2.Discover(ctx, focusrite2.Config{})
		if e == nil {
			stored := s.opts.Store.Get().Focusrite.Control2
			cfg.PrivateKey = stored.PrivateKey
			cfg.Outputs = stored.Outputs
			cfg.MasterMute = stored.MasterMute
			cfg.MasterDim = stored.MasterDim
			cfg.DeviceSerial = stored.DeviceSerial
			cfg, e = cfg.WithIdentity()
		}
		if e == nil {
			// Save before requesting approval, so closing the app cannot lose the
			// identity that FC2 has just approved. The settings file has mode 0600.
			s.opts.Store.Update(func(c *config.Config) { c.Focusrite.Control2 = cfg })
			e = s.opts.Store.Save()
		}
		var bridge *focusrite2.Bridge
		if e == nil {
			bridge, e = focusrite2.StartBridge(ctx)
		}
		if e != nil {
			fyne.Do(func() { status.SetText(e.Error()); content.Add(widget.NewButton("Close", closePair)) })
			return
		}
		update := func(message string) { bridge.Status(message); fyne.Do(func() { status.SetText(message) }) }
		fyne.Do(func() {
			s.draft.Focusrite.Control2 = cfg
			choices := newSelect(bridge.URLs, nil)
			choices.Selected = bridge.URLs[0]
			image := canvas.NewImageFromResource(nil)
			image.SetMinSize(fyne.NewSize(256, 256))
			image.FillMode = canvas.ImageFillContain
			image.ScaleMode = canvas.ImageScalePixels
			link := widget.NewHyperlink("Open mobile pairing page", nil)
			show := func(address string) {
				png, e := qrcode.Encode(address, qrcode.Medium, 512)
				if e != nil {
					dialog.ShowError(e, win)
					return
				}
				image.Resource = fyne.NewStaticResource("fc2-pair.png", png)
				image.Refresh()
				link.URL, _ = url.Parse(address)
			}
			choices.OnChanged = show
			show(choices.Selected)
			hint := widget.NewLabel("Approve Sweetspot in FC2. Then take a screenshot of the QR code FC2 shows (⇧⌘4) and load it here, or drop the file onto this window.\n\nOr with your phone: scan this Sweetspot QR, accept the self-signed HTTPS certificate, then scan FC2's QR with the mobile page.")
			hint.Wrapping = fyne.TextWrapWord
			shotInfo := widget.NewLabel("")
			shotInfo.Wrapping = fyne.TextWrapWord
			load := func(uri fyne.URI) {
				shotInfo.SetText("Reading " + uri.Name() + "…")
				go func() {
					e := submitQRScreenshot(bridge, uri)
					fyne.Do(func() {
						if e != nil {
							shotInfo.SetText(e.Error())
						} else {
							shotInfo.SetText("")
						}
					})
				}()
			}
			loadButton := widget.NewButton("Load QR screenshot…", func() {
				d := dialog.NewFileOpen(func(r fyne.URIReadCloser, e error) {
					if e != nil || r == nil {
						return
					}
					_ = r.Close()
					load(r.URI())
				}, win)
				d.SetFilter(storage.NewExtensionFileFilter([]string{".png", ".jpg", ".jpeg"}))
				d.Show()
			})
			win.SetOnDropped(func(_ fyne.Position, uris []fyne.URI) {
				if len(uris) > 0 {
					load(uris[0])
				}
			})
			content.Add(hint)
			content.Add(loadButton)
			content.Add(shotInfo)
			content.Add(image)
			certificateHint := widget.NewLabel("This pairing session uses a self-signed HTTPS certificate. SHA-256 fingerprint:\n" + bridge.CertificateFingerprint)
			certificateHint.Wrapping = fyne.TextWrapBreak
			content.Add(certificateHint)
			content.Add(choices)
			content.Add(link)
			content.Add(widget.NewButton("Close", closePair))
		})
		e = focusrite2.Pair(ctx, cfg, bridge.QR, update)
		bridge.Finish(e)
		if e != nil {
			update(fmt.Sprintf("Pairing failed: %v", e))
			return
		}
		s.opts.Store.Update(func(c *config.Config) { c.Focusrite.Backend = "control2"; c.Focusrite.Control2 = cfg })
		if e = s.opts.Store.Save(); e != nil {
			update("Paired, but could not save settings: " + e.Error())
			return
		}
		fyne.Do(func() {
			s.draft.Focusrite.Backend = "control2"
			s.draft.Focusrite.Control2 = cfg
			s.buildFocusrite()
			status.SetText("Paired with Focusrite Control 2. You can close this window.")
		})
		if s.opts.Apply != nil {
			s.opts.Apply()
		}
	}()
}

// maxScreenshotBytes bounds a loaded QR screenshot, like the mobile upload.
const maxScreenshotBytes = 25 << 20

// submitQRScreenshot reads FC2's pairing QR from an image file and hands it
// to the pairing session, the same way the mobile page does.
func submitQRScreenshot(bridge interface{ Submit(string) error }, uri fyne.URI) error {
	r, e := storage.Reader(uri)
	if e != nil {
		return e
	}
	defer r.Close()
	b, e := io.ReadAll(io.LimitReader(r, maxScreenshotBytes+1))
	if e != nil {
		return e
	}
	if len(b) > maxScreenshotBytes {
		return errors.New("image too large; crop the screenshot to the QR code")
	}
	value, e := focusrite2.DecodeQRImage(bytes.NewReader(b))
	if e != nil {
		return e
	}
	return bridge.Submit(value)
}
