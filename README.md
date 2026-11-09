# sinty-cored

The Sinty Core service of Sinty OS. A Sinty Core is a small computer that runs
your desktop and shows it on paired screens: a TV, a monitor with a thin
client, a laptop shell. Screens send keyboard and pointer input back.

`sinty-cored` runs on the Core. `sinty-screen` runs on a screen and draws on
the Linux framebuffer, reading keyboards and pointers from `/dev/input`.

## How it works

- **Pairing.** Settings on the Core opens a pairing window. The screen shows a
  six digit code, the person types it on the Core. Both sides then hold each
  other's key, and the Core asks `sinty-keyd` to trust the screen.
- **Sessions.** Every session starts with a fresh key exchange signed by both
  identities, then runs AES-256-GCM in each direction. Removing a screen in
  Settings, or revoking it in `sinty-keyd`, ends its session at once.
- **Picture.** The Core captures the signed-in desktop with
  `wlr-screencopy`, waits for damage, and sends only the 64x64 tiles that
  changed, compressed in software. Input arrives through the virtual pointer
  and virtual keyboard protocols, with the screen's keyboard layout.
- **Links.** A screen finds the Core on the local network over UDP. Without a
  network the Core opens a Wi-Fi Direct group through `wpa_supplicant`. With a
  USB-C cable it shows up as a network adapter, or as a disk.
- **Phone.** When the device form is `headless`, the Core announces itself
  over Bluetooth LE with a presence characteristic and a write-only control
  characteristic, both encrypted.
- **Power.** While a screen is showing the desktop the Core tells
  `sinty-powerd` it is streaming.

## D-Bus

`dev.sinty.Core1` on the system bus, object `/dev/sinty/Core1`. Methods are
allowed to root and to the person signed in on the Core.

| Member | Signature | |
|---|---|---|
| `Name`, `ID` | property `s` | |
| `Screens` | property `a(ssb)` | Paired screens: id, name, connected. |
| `Pairing`, `PairingScreen` | property `b`, `s` | |
| `Route` | property `s` | `cable`, `network`, `direct` or `none`. |
| `Direct` | property `s` | Name of the open Wi-Fi Direct network. |
| `Phone` | property `s` | Last phone that sent a command. |
| `UsbMode` | property `s` | `off`, `network` or `disk`. |
| `Form` | property `s` | Device form reported by `sinty-powerd`. |
| `Encoder` | property `s` | |
| `StartPairing()` | out `s` | Opens the window for two minutes, returns the route. |
| `ConfirmPairing(code)` / `CancelPairing()` | | |
| `RevokeScreen(id)` | | |
| `SetUsbMode(mode)` / `SetForm(form)` | | |
| `DirectNetwork()` | out `ss` | Name and passphrase of the Wi-Fi Direct network. |
| `PairingRequest(screen)` | signal | |
| `PairingFinished(screen, result)` | signal | `paired`, `wrong-code`, `cancelled` or `failed`. |
| `PhoneCommand(device, command)` | signal | |

Errors are `dev.sinty.Core1.Error.` followed by `NotAllowed`, `NotPairing`,
`NoLink`, `NotSupported` or `Failed`.

Screens connect on TCP port 7412 and look for the Core on UDP port 7413.

## Building

```sh
make
make test
make install DESTDIR=...
```

Requires Go >= 1.25 and Linux. The tests need `dbus-daemon`.
