# AGENTS.md

Guide for coding agents (and humans) working on android-display: an Android phone/tablet as an extended display for macOS, over USB (adb reverse) or WiFi.

## Layout

| Path | What |
|---|---|
| `server/` | Go + cgo/Objective-C server for the Mac. Creates a `CGVirtualDisplay` (private API), captures it, encodes H.264 with VideoToolbox, streams it, injects touch as mouse events. |
| `server/cmd/server/main.go` | Entry point, flags, pipeline wiring, display position persistence. |
| `server/internal/control` | TCP handshake (ClientHello → ServerHello → ClientReady). The server picks stream size and bitrate here. |
| `server/internal/display` | Virtual display creation (`display_bridge.m`), origin get/set. |
| `server/internal/input` | Touch → mouse injection (`injector.go`, `input_bridge.m`) and gesture recognition. |
| `server/internal/protocol` | JSON handshake messages. Must stay in sync with `client/.../protocol/Messages.kt`. |
| `client/` | Android app (Kotlin). Receives video (UDP for WiFi, TCP for USB), decodes with MediaCodec, sends normalised touch. |
| `mac-app/` | SwiftUI front-end (`AndroidDisplayApp.swift`) that bundles and supervises the server; `build.sh` builds it. |

## Build

Requirements: macOS 12.3+ (mac app: 13+), Xcode command line tools, Go 1.21+, `adb` (`brew install android-platform-tools`), JDK 17 or 21 and Android SDK 34 for the client.

```bash
# Server only
cd server && go build -o android-display-server ./cmd/server

# macOS app (builds the server too) → ~/Applications/AndroidDisplay.app
mac-app/build.sh

# Android client — Gradle 8.5 / AGP 8.2 need JDK 17 or 21 (not 25)
cd client
export JAVA_HOME=$(/usr/libexec/java_home -v 21)   # or the Homebrew openjdk@21 path
echo "sdk.dir=$ANDROID_HOME" > local.properties     # if the SDK is not auto-detected
./gradlew assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
```

`swiftc` needs `-enable-bare-slash-regex` for the regex literals in the mac app (already in `build.sh`).

## Run

```bash
./server/android-display-server -scale 0.75 -bitrate 4000000
```

- `-scale` (0.25–1): streamed size as a fraction of the device's native screen. `-bitrate` in bps (default 8 Mbps). These flags are only the fallback for phones the settings file doesn't cover. The Android app has no quality settings; it only reports its native size.
- Per-phone settings: the server re-reads `~/Library/Application Support/AndroidDisplay/device-settings.json` on every handshake (the mac app writes it). Precedence per value: the phone's own entry, the file's `defaults`, the flags. Out-of-range values are ignored. A change applies the next time that phone connects; no server restart.
- `-exit-on-stdin-eof`: shut down when stdin closes. The mac app uses this so the server never outlives it. While it is set, stdin also carries commands, one per line: `reconnect <session id>` makes that phone reconnect by itself so new settings apply (the server sends `{"type":"reconnect"}` on the phone's control channel; the phone's service hands over to `DisplayActivity`, which starts `MainActivity` with the same connection type, and that taps Connect after 800 ms). To try it without the app, run the server with a FIFO as stdin (`mkfifo f; server -exit-on-stdin-eof <>f`) and `echo "reconnect 1" > f`.
- Several phones at once: each connection gets its own session (id from the handshake) with its own virtual display, encoder, capture and touch routing. Touch and TCP video use one shared port each; the phone sends its 4-byte big-endian session id first. `adb reverse` is set up for every attached device, and a phone plugged in later is picked up within 2 s.
- On the phone, pick USB or WiFi and tap Conectar.

Logs: the mac app writes the server log to `~/Library/Logs/AndroidDisplay.log` (truncated on each app launch). Per-session lines start with `session N:`. The app UI state is derived by parsing those lines (`ServerController.handle(line:)`). If you change a server log message, update the parser.

## macOS permissions (common source of "it doesn't work")

- **Screen Recording** is needed to capture the virtual display. Without it the log shows `CGDisplayStreamCreate failed … (after 10 attempts)`.
- **Accessibility** is needed to inject touches. Without it the log shows `WARNING: Accessibility permission not granted!` and touches are silently dropped.
- Permissions belong to whatever launches the server: the terminal when run by hand, `AndroidDisplay.app` when run from the app.
- TCC binds permissions to the code signature. Ad-hoc signatures change on every build, so grants stop matching (`log show --predicate 'subsystem == "com.apple.TCC"'` shows `Failed to match existing code requirement`). `build.sh` signs with a keychain identity named `AndroidDisplay Local Signing` when present, which keeps grants across rebuilds. Create one with a self-signed code-signing certificate (EKU `codeSigning`) imported into the login keychain with `-T /usr/bin/codesign`.
- Reset stale grants: `tccutil reset Accessibility local.futit.androiddisplay` (same for `ScreenCapture`). macOS needs an app relaunch after granting Screen Recording.

## Gotchas

- The virtual display can move after it is created (macOS restoring the arrangement, user dragging it). Never cache its bounds; query `CGDisplayBounds` when needed.
- The display needs a fixed non-zero `serialNum`. With 0, macOS gives it a random UUID per connection and forgets its arrangement.
- Each phone's display position is saved in global coordinates, keyed by its device id (the Android ID the phone sends), in `~/Library/Application Support/AndroidDisplay/display-origins.json`, and restored on its next connection. If other displays are rearranged, the saved spot may be stale. Moving it once in System Settings fixes it. The display serial is derived from the same id, so each phone is a separate monitor.
- Changing a phone's scale or bitrate requires reconnecting it: the client sizes its decoder from ServerHello.
- Port 9000 is fixed. "address already in use" means another server instance is still running.
- While streaming, `DisplayActivity` sets `FLAG_KEEP_SCREEN_ON`, so the phone neither dims nor sleeps. To keep the connect screen awake too, use `adb shell settings put global stay_on_while_plugged_in 7`. Some phones report USB charging as AC, so `2` (USB only) does nothing on them.
- Qualcomm decoders (`c2.qti.avc.decoder` and friends) ignore `KEY_LOW_LATENCY` unless they declare the feature. Without `vendor.qti-ext-dec-low-latency.enable` / `vendor.qti-ext-dec-picture-order.enable` they hold about 8 frames (~130 ms). Keep those keys in `VideoDecoder.start()`. Those keys are for Codec2 (Android 10+); the OMX decoder on Android 5 (`OMX.qcom.video.decoder.avc`) ignores them, so the stream itself has to say it needs no buffering:
  - VideoToolbox's SPS has no VUI, so a decoder must assume it may hold as many frames as the level allows (10 at 896x540 level 3.2) and the OMX one only outputs when that buffer is full. On a desktop that changes a few times per second the picture arrived 13–26 s late (measured with the `Latency` log: `decoded − recv`). `h264.LimitDecoderBuffering` rewrites the SPS to add a VUI with `max_num_reorder_frames=0` and `max_dec_frame_buffering=max_num_ref_frames`. It leaves SPSs it does not understand, or that already have a VUI, untouched.
  - That decoder then still keeps the *newest* frame until the next one arrives, so the last change of a burst stayed invisible while the screen was quiet. `frameFlusher` re-encodes the last captured frame 30 ms after it unless a new one arrives (a few dozen bytes: the encoder codes it as unchanged).
  - Result on the XT1025: 13–26 s late → about 28 ms on average, 45 ms worst-case p95 over 23 two-second windows. The moto g(9) plus was unaffected (11–18 ms).
- **Known issue:** touch injection fails when a phone's display is placed to the left of (or above) the main display, i.e. at negative global coordinates. The cursor ends up on the main display's edge instead of the phone's display. macOS accepts `CGWarpMouseCursorPosition` there, but a posted click event does not stay at a negative `x`. Positive coordinates work. Workaround: arrange the phone's display to the right of the main one.
- **Touch modes** (`defaults.touchMode` in `device-settings.json`, picked in the Mac app, read when a phone connects; `internal/input`). macOS has one cursor, so by default (`cursor`) a touch moves it to the phone's display and clicks there, which takes it away from the user.
  - `restore`: same, and the cursor goes back to where it was when the gesture ends.
  - `background`: each touch goes only to the app whose window is under the finger, and the cursor is never touched. What has no window to target (menu bar, Dock, desktop: window layer ≠ 0) falls back to `restore`. `pointerRouter` (pure Go, tested with a fake macOS in `router_test.go`) decides; `cgOps` and `input_bridge.m` do the CoreGraphics/SkyLight part.
  - What `background` needs on macOS 26.2, all found by trial against a test window (a bare `CGEventPostToPid` of a click is silently dropped): deliver with SkyLight's private `SLEventPostToPid` (looked up with `dlsym`; the public call is the fallback); stamp the event with the window number (field 51; without it `NSEvent.window` is nil), the target pid (field 40) and the point inside the window with the private `CGEventSetWindowLocation` (on 26.2 it is taken as the window-local point, top-left origin); send a `mouseMoved` before each press; and make the window key without raising it with two `SLPSPostEventRecordTo` records (the yabai pattern), because an inactive app ignores mouse events for its views. That last step costs about 60 ms, so it is repeated only when the touched window changes or after 5 s. If any private symbol is missing, priming does nothing and background touches may be ignored.
  - Verified with a purpose-built AppKit window on the phone's display, driven by taps from a real phone: move, press and release arrive at the touched point with the real cursor unmoved, drags and the release keep going to the window when the finger leaves it, a long press gives a right click, and the frontmost app does not change. **Not verified:** real apps (Safari, Finder, Chrome, games), two-finger scroll end to end, double click end to end (`adb input tap` is ~730 ms apart, longer than the 500 ms double-click window; it is unit tested), macOS versions other than 26.2, and display positions at negative coordinates. Chrome is known to filter events that do not come with real-input bookkeeping, and canvas apps and games filter per-process events altogether, so those may ignore `background` touches.
  - Posting events needs Accessibility permission for the posting process; a process without it (e.g. a test binary run from an agent's shell) has its events dropped, with no error. Test through `AndroidDisplay.app`, not a standalone server.
  - Re-signing `AndroidDisplay.app` needs the keychain to allow `codesign` to use the `AndroidDisplay Local Signing` key; a pending approval dialog makes `codesign` hang with no output (`.cstemp` files left in the bundle). Approve it once with "Always Allow".
- Android ID differs per app, so `adb shell settings get secure android_id` does not match the id the app sends. Read it from the `id=` field of the `client connected` log line.
- On Android 5.1 (legacy variant) `adb shell input tap` uses the landscape logical coordinates, while `screencap` gives the unrotated framebuffer. Get button positions with `adb shell uiautomator dump` instead of reading them off the screenshot.

## Tests

```bash
cd server && go test ./...
```

`internal/control` covers the handshake (server-side scaling, per-phone settings, distinct session ids), `internal/touch` and `internal/stream` the routing of several phones by session id, `internal/adb` the parsing of `adb devices`, and `cmd/server` the per-phone settings precedence and position store. `internal/input` has one test that already fails upstream (`TestMoveWithinThresholdDoesNotDrag`); don't treat it as a regression from your change. Use `go test -vet=off` if `go vet` is killed on your machine.

## Verifying end to end without touching the phone

With the phone on USB and USB debugging enabled:

```bash
adb shell monkey -p com.androidmac.client -c android.intent.category.LAUNCHER 1   # open app
adb exec-out screencap -p > /tmp/phone.png                                        # see its screen
adb shell input tap <x> <y>                                                       # tap USB, then Conectar
```

Then check the log for `pipeline active (video + touch)`. To check touch, tap the centre of the phone and read the cursor position (e.g. a 3-line Swift script printing `CGEvent(source: nil)!.location`). It should land inside the virtual display's `CGDisplayBounds`.

## Measuring latency

Each packet header carries the frame's capture time on the Mac (Unix µs, derived from the `CGDisplayStream` display time). Both sides log a summary every 2 s:

- Mac (server log): `latency mac: encode avg … p95 … ms | send avg … | fps | Mbps`. Encode is measured from the frame's scheduled display time, so it can be slightly negative.
- Phone (`adb logcat -s Latency`): `recv avg … | decoded avg … | fps | decoder dropped | queue max`.

The phone values include the Mac↔phone clock offset, which can be minutes. `decoded − recv` doesn't depend on it and is the decoder's own delay. For absolute numbers, measure the offset over one persistent `adb shell` session: send `date +%s%N` repeatedly and use phone time minus the midpoint of the Mac send/receive times on the lowest-RTT samples (about ±8 ms). Then subtract the offset. Generate steady motion on the virtual display (e.g. a full-window canvas animation) while measuring. A static desktop only produces frames on change.

## Conventions

- Keep the Go and Kotlin protocol structs in sync. The server is the source of truth for stream parameters.
- Run `gofmt` on Go code. Don't add Go dependencies without a reason.
- Upstream comments are partly in Traditional Chinese, and the Android UI strings are in Spanish in this fork. Keep user-facing text in `strings.xml`.
