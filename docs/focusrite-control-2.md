# Focusrite Control 2

Sweetspot has a separate FC2 transport. Focusrite Control 1 remains available
under **Configuration → Focusrite → Focusrite Control**.

## Pairing with a screenshot

Pairing runs in a modal dialog over the settings; the other settings cannot
be changed until it is closed. FC2 shows its pairing QR code on the same
computer, so no phone is needed:

1. In Sweetspot, open **Configuration → Focusrite**, select
   **Focusrite Control 2**, and click **Pair with Focusrite Control 2…**.
2. Approve **Sweetspot** in the FC2 desktop app. FC2 then displays its own
   pairing QR code.
3. Take a screenshot of the QR code (macOS: ⇧⌘4 and drag over the code; a
   screenshot of the whole window works too).
4. In the pairing dialog, click **Load QR screenshot…** and choose the file,
   or drop the file onto the settings window.
5. Wait for **Paired with Focusrite Control 2**; the pairing dialog then
   closes on its own.

The screenshot is decoded locally with the same decoder and checks as a phone
upload; a code is accepted once per pairing session.

## Pairing with a phone

1. Run FC2 on the same computer as Sweetspot. Connect the phone to the same
   Wi-Fi/LAN as the computer.
2. In Sweetspot, open **Configuration → Focusrite**, select
   **Focusrite Control 2**, and click **Pair with Focusrite Control 2…**.
3. Sweetspot starts a temporary LAN web server and shows a QR code containing
   its mobile pairing URL. Scan this code with the phone's normal camera.
   If several network addresses are listed, choose the computer's Wi-Fi or
   Ethernet address. The link uses HTTPS with a self-signed certificate.
   Accept the browser certificate warning for this local Sweetspot session.
   Its SHA-256 fingerprint is shown in the Sweetspot pairing dialog.
4. Approve **Sweetspot** in the FC2 desktop app. FC2 then displays its own
   pairing QR code.
5. On the mobile Sweetspot page, tap **Scan with camera**, allow camera access,
   and point the camera at the complete FC2 QR code. Scanning stops when the
   code is submitted. If camera access remains blocked, open **Use a photo
   instead**, select or photograph the code. Selection uploads the photo automatically and
   shows progress below the picker. **Send QR photo** also allows a manual retry.
   Decoded hexadecimal data can alternatively be pasted under **Enter decoded
   QR data**.
6. Wait for **Paired with Focusrite Control 2**. Uploading the photo alone
   is not a successful pairing: FC2 must verify the payload.

The mobile page is self-contained and makes no CDN requests. Each pairing
session creates an in-memory self-signed TLS certificate with SANs for the local
IP addresses. No certificate or key is installed in a system trust store.
Photo upload and manual code entry are native HTML forms and also work without
JavaScript; a server-rendered status page shows FC2 verification. The live scanner
is only shown once its separate same-origin script has initialized.
Browser certificate exceptions and camera permissions are separate: depending
on browser policy, camera access may require explicitly trusting the certificate
and reloading. The photo picker remains available if camera access is blocked.

Browsers with QR `BarcodeDetector` support decode the live video on the phone.
Other browsers send reduced JPEG camera frames to Sweetspot's HTTPS scan endpoint,
where the same local decoder used for photo uploads reads the QR code. Frames
are processed in memory and discarded; they are never sent to a third party.

Closing the pairing dialog cancels its network activity. Pairing expires after
five minutes. The LAN server stops ten seconds after success or failure, allowing
the phone to receive the result. Each session uses a random 256-bit URL token.
The page only transfers QR data; it does not expose credentials or audio controls.
The client private key stays in Sweetspot's owner-readable settings file.
Until a pairing succeeds, only that key is saved: the previously trusted FC2
identity stays pinned when a pairing is cancelled or fails, and mappings saved
before are kept.

## Assigning controls on any interface

After pairing, Sweetspot reads FC2's AES70 object tree. There are no hardcoded
interface model names or monitor/output object numbers in control discovery.
The list includes each object's hierarchical role, class, and object number.

For each speaker A–D and the subwoofer, choose one **Output / mix**.
Sweetspot assigns its gain, mute and available left/right meters together.
Direct outputs with gain and mute controls appear first. If FC2 exposes no
controllable direct output, select **Mix A**, **Mix B**, etc. and route that mix
to the speaker's physical outputs in FC2. Each speaker needs a distinct target;
changing a mix affects every physical output fed by it. Input-channel gains
inside a mix are excluded from the selector.

The choices come from the discovered object tree, so the mix count is not
limited to the three buses on a 4i4. Incomplete or ambiguous control groups are
omitted. Existing manual assignments are retained until explicitly replaced.
Then **Save**. Stereo linking, routing and which outputs are under hardware
volume control remain configured in FC2.

As with Focusrite Control 1, each speaker takes its name from FC2: the custom
name of its mix or output (`CustomName`), or its fixed name when none is set.
Sweetspot reads the names on connect and every two seconds, so renaming a mix
in FC2 renames the speaker.

You can also map monitor Mute and FC2 Dim; both lists offer only the
`MainOutputs` controls. Dim works as with Focusrite Control 1: Sweetspot
lowers the assigned output gains by its configured dim amount, and FC2's
`MainOutputs / Dim` follows in both directions. FC2's own Dim does not reach
outputs fed by mixes; an output that is also under FC2's hardware volume
control is dimmed by both. Values below a device's supported gain range use
output mute.

Unassigned elements are never written. Unmapped speakers, meters and master
controls are disabled in the desktop UI. Sweetspot checks saved object paths,
numbers, classes, device serials (when advertised), duplicate output assignments,
and live gain limits before writing. Changed mappings need to be selected again.
On reconnect, the selected physical output's current gain is adopted before any
saved monitor state is pushed, using the dim state it was written with; FC2's
Mute and Dim are applied after that. A gain that carried dim or sat at the
lower limit never yields a volume above the saved one, so un-dimming or
un-muting after a reconnect is never louder than before. Only speakers with
an assigned output count as the volume and meter source.

The implementation is model-independent, but the functions available depend on
what a given FC2 version/interface publishes through AES70. Vendor-specific
classes without a recognized standard base class are not guessed. Use **Copy
discovered controls** to collect the inventory when an expected control is absent.
Manual hardware checks passed with a **Scarlett 4i4 (3rd Gen)**, including
pairing, assignments, volume/mute/dim, scenes, meters, persistence and reconnects.
Other physical interfaces, including the 18i20, still need hardware validation.
Hardware-free tests cover different
model names, object-number ranges, mapping changes, output writes, QR pairing,
and encrypted transport interoperability. They do not establish compatibility
with every physical interface.

## Connection and protocol

FC2 advertises `_ocaws._tcp.local.`. Sweetspot accepts local-computer entries,
reads the onboarding port plus `securePort` and `publicKey` TXT records, and pins
the server identity. Ports are rediscovered on reconnect; a changed public key
requires explicit pairing again.

Onboarding uses FC2's authentication agent over AES70/OCP.1 WebSocket, X25519
client identities and NaCl box encryption of the client name. Approved QR bytes
are submitted on the same onboarding connection, kept alive while the phone is
used. The approved identity then opens the secure WebSocket. Its session uses
Libsodium-compatible XChaCha20-Poly1305 secretstream records and a nonce echo
handshake before any AES70 control traffic. Control reads/writes are serialized,
and receive buffers are bounded across fragmented WebSocket messages.

Protocol references:

- [Focusrite's pairing troubleshooting](https://support.focusrite.com/hc/en-gb/articles/21402515137042-I-m-having-problems-pairing-Focusrite-Control-2-for-iOS-and-Android-to-my-desktop-computer)
- [Independent FC2 protocol research and interoperability reference](https://github.com/alexanderdalesio/focusrite-control-2-api/blob/main/docs/PROTOCOL.md)
- [Libsodium secretstream reference implementation](https://github.com/jedisct1/libsodium/blob/master/src/libsodium/crypto_secretstream/xchacha20poly1305/secretstream_xchacha20poly1305.c)

The Go transport is independently implemented. Committed cryptographic fixtures
were generated with `libsodium-wrappers` 0.8.4 and cover key derivation, both stream
directions, non-aligned records, rekey, final tags, and authentication failures.
