// Android Display — Mac front-end for android-display-server.
// Runs the bundled server as a child process, lists the connected phones parsed from its
// log, lets you set the resolution and bitrate of each phone, and stops the server on quit.
//
// Settings live in ~/Library/Application Support/AndroidDisplay/device-settings.json. The
// server re-reads that file on every handshake, so a change applies the next time the phone
// connects, without restarting the server.

import AppKit
import ApplicationServices
import CoreGraphics
import SwiftUI

private let scaleOptions: [Double] = [1.0, 0.9, 0.75, 0.6, 0.5, 0.4, 0.3]
private let bitrateOptions: [Int] = [12_000_000, 8_000_000, 6_000_000, 4_000_000, 2_000_000, 1_000_000]

private func scalePercent(_ s: Double) -> String { "\(Int((s * 100).rounded())) %" }
private func bitrateLabel(_ b: Int) -> String { "\(b / 1_000_000) Mbps" }

/// Streamed size for a native size at a scale; even dimensions, like the server (H.264 needs them).
private func scaledSize(_ native: (width: Int, height: Int), _ s: Double) -> String {
    "\(Int(Double(native.width) * s) & ~1)×\(Int(Double(native.height) * s) & ~1)"
}

// MARK: - Settings

struct StreamDefaults: Codable, Equatable {
    var scale: Double
    var bitrate: Int
    /// How touches reach macOS: "cursor" (default, also when nil), "restore" or "background".
    var touchMode: String?
}

/// One phone's own settings. A nil scale/bitrate means "use the default".
struct PhoneSettings: Codable, Equatable {
    var name: String
    var scale: Double?
    var bitrate: Int?
    var nativeWidth: Int?
    var nativeHeight: Int?

    var nativeSize: (width: Int, height: Int)? {
        guard let w = nativeWidth, let h = nativeHeight else { return nil }
        return (w, h)
    }
}

struct SettingsFile: Codable {
    var defaults: StreamDefaults
    var devices: [String: PhoneSettings]
}

@MainActor
final class SettingsStore: ObservableObject {
    static let shared = SettingsStore()

    @Published private(set) var file: SettingsFile

    private let url = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Application Support/AndroidDisplay/device-settings.json")

    private init() {
        if let data = try? Data(contentsOf: url), let loaded = try? JSONDecoder().decode(SettingsFile.self, from: data) {
            file = loaded
        } else {
            // First run with this file: carry over the global choice older versions kept in preferences.
            let prefs = UserDefaults.standard
            let scale = prefs.object(forKey: "scale") as? Double ?? 0.75
            let bitrate = prefs.object(forKey: "bitrate") as? Int ?? 4_000_000
            file = SettingsFile(defaults: StreamDefaults(scale: scale, bitrate: bitrate), devices: [:])
            save()
        }
    }

    var defaults: StreamDefaults { file.defaults }

    func scale(for id: String) -> Double { file.devices[id]?.scale ?? file.defaults.scale }
    func bitrate(for id: String) -> Int { file.devices[id]?.bitrate ?? file.defaults.bitrate }

    func setDefaults(scale: Double? = nil, bitrate: Int? = nil, touchMode: String? = nil) {
        if let scale { file.defaults.scale = scale }
        if let bitrate { file.defaults.bitrate = bitrate }
        if let touchMode { file.defaults.touchMode = touchMode }
        save()
    }

    func setScale(_ scale: Double?, for id: String) {
        guard file.devices[id] != nil else { return }
        file.devices[id]?.scale = scale
        save()
    }

    func setBitrate(_ bitrate: Int?, for id: String) {
        guard file.devices[id] != nil else { return }
        file.devices[id]?.bitrate = bitrate
        save()
    }

    /// Remembers a phone the first time it connects, and refreshes its name and native size.
    func noteConnected(id: String, name: String, width: Int, height: Int) {
        guard !id.isEmpty else { return }
        var phone = file.devices[id] ?? PhoneSettings(name: name)
        phone.name = name
        phone.nativeWidth = width
        phone.nativeHeight = height
        guard file.devices[id] != phone else { return }
        file.devices[id] = phone
        save()
    }

    func forget(_ id: String) {
        file.devices[id] = nil
        save()
    }

    private func save() {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        do {
            try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            try encoder.encode(file).write(to: url, options: .atomic) // the server may read it at any moment
        } catch {
            ServerController.shared.report("No se pudieron guardar los ajustes: \(error.localizedDescription)")
        }
    }
}

// MARK: - Server

enum ServerState {
    case stopped, running
}

/// One connected phone, as reported by the server's "session N: …" log lines.
struct PhoneSession: Identifiable, Equatable {
    let id: Int
    var deviceId: String
    var device: String
    var streamSize: String?
    var appliedScale: Double?
    var appliedBitrate: Int?
    var isStreaming = false
}

@MainActor
final class ServerController: ObservableObject {
    static let shared = ServerController()

    @Published private(set) var state: ServerState = .stopped
    @Published private(set) var sessions: [PhoneSession] = []
    /// Native screen size of the most recently connected phone; used to preview the pixel sizes.
    @Published private(set) var nativeSize: (width: Int, height: Int)?
    /// Serials of the phones adb currently sees; the server forwards their ports as they appear.
    @Published private(set) var usbDevices: Set<String> = []
    @Published private(set) var error: String?

    let logURL = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Logs/AndroidDisplay.log")

    private var process: Process?
    private var stdinPipe: Pipe?
    private var outputPipe: Pipe?
    private var lineBuffer = Data()

    private init() {
        FileManager.default.createFile(atPath: logURL.path, contents: nil)
    }

    func report(_ message: String) { error = message }

    /// Asks a connected phone to reconnect by itself, so its new settings take effect.
    /// The server forwards it to the phone, which drops the session and handshakes again.
    func reconnect(session id: Int) {
        guard let pipe = stdinPipe else { return }
        try? pipe.fileHandleForWriting.write(contentsOf: Data("reconnect \(id)\n".utf8))
    }

    /// Starts the server. The scale/bitrate flags are only what a phone gets when the settings
    /// file doesn't cover it; the file itself is what normally decides.
    func start() {
        guard process == nil else { return }
        guard let binary = Bundle.main.url(forResource: "android-display-server", withExtension: nil) else {
            error = "No se encontró android-display-server dentro de la app."
            return
        }
        let defaults = SettingsStore.shared.defaults

        let proc = Process()
        proc.executableURL = binary
        proc.arguments = [
            "-scale", String(defaults.scale),
            "-bitrate", String(defaults.bitrate),
            "-exit-on-stdin-eof",
        ]
        var env = ProcessInfo.processInfo.environment
        env["PATH"] = "/opt/homebrew/bin:/usr/local/bin:" + (env["PATH"] ?? "/usr/bin:/bin")
        proc.environment = env

        // The server exits when this pipe closes, so it never outlives the app.
        let stdin = Pipe()
        proc.standardInput = stdin
        let output = Pipe()
        proc.standardOutput = output
        proc.standardError = output

        let logURL = self.logURL
        output.fileHandleForReading.readabilityHandler = { handle in
            let data = handle.availableData
            guard !data.isEmpty else {
                handle.readabilityHandler = nil // EOF: the server exited
                return
            }
            if let log = try? FileHandle(forWritingTo: logURL) {
                log.seekToEndOfFile()
                log.write(data)
                try? log.close()
            }
            Task { @MainActor in ServerController.shared.consume(data) }
        }

        proc.terminationHandler = { p in
            Task { @MainActor in ServerController.shared.didTerminate(p) }
        }

        do {
            try proc.run()
        } catch {
            self.error = "No se pudo iniciar el servidor: \(error.localizedDescription)"
            return
        }
        process = proc
        stdinPipe = stdin
        outputPipe = output
        error = nil
        state = .running
    }

    /// Stops the server and waits (up to ~3 s) for it to clean up its adb forwards.
    func stop() {
        guard let proc = process else { return }
        // Detach first: late exit/log events from this process must not touch the next one.
        proc.terminationHandler = nil
        proc.terminate()
        let deadline = Date().addingTimeInterval(3)
        while proc.isRunning && Date() < deadline {
            usleep(50_000)
        }
        if proc.isRunning {
            kill(proc.processIdentifier, SIGKILL)
            proc.waitUntilExit()
        }
        outputPipe?.fileHandleForReading.readabilityHandler = nil
        try? stdinPipe?.fileHandleForWriting.close()
        reset()
    }

    private func reset() {
        process = nil
        stdinPipe = nil
        outputPipe = nil
        state = .stopped
        sessions = []
        usbDevices = []
        lineBuffer.removeAll()
    }

    private func didTerminate(_ proc: Process) {
        // Only an unexpected exit of the current server gets here; stop() detaches its handler.
        guard proc === process else { return }
        let status = proc.terminationStatus
        if error == nil {
            error = "El servidor se cerró (código \(status)). Revisá el log."
        }
        reset()
    }

    private func consume(_ data: Data) {
        lineBuffer.append(data)
        while let newline = lineBuffer.firstIndex(of: UInt8(ascii: "\n")) {
            let lineData = lineBuffer[lineBuffer.startIndex..<newline]
            lineBuffer.removeSubrange(lineBuffer.startIndex...newline)
            handle(line: String(decoding: lineData, as: UTF8.self))
        }
    }

    private func handle(line: String) {
        if line.contains("control server on port") {
            state = .running
        } else if let m = line.firstMatch(of: /session (\d+): client connected: (.+) \((\d+)x(\d+)\) \[\w+\] id=(\S*)/) {
            let id = Int(m.1) ?? 0
            let native = (width: Int(m.3) ?? 0, height: Int(m.4) ?? 0)
            sessions.removeAll { $0.id == id }
            sessions.append(PhoneSession(id: id, deviceId: String(m.5), device: String(m.2)))
            nativeSize = native
            SettingsStore.shared.noteConnected(id: String(m.5), name: String(m.2), width: native.width, height: native.height)
        } else if let m = line.firstMatch(of: /session (\d+): stream config: (\d+)x(\d+) @ (\d+) bps \(scale ([0-9.]+)\)/) {
            update(session: Int(m.1) ?? 0) {
                $0.streamSize = "\(m.2)×\(m.3)"
                $0.appliedBitrate = Int(m.4)
                $0.appliedScale = Double(m.5)
            }
        } else if let m = line.firstMatch(of: /session (\d+): pipeline active/) {
            update(session: Int(m.1) ?? 0) { $0.isStreaming = true }
            error = nil
        } else if let m = line.firstMatch(of: /session (\d+): (client disconnected|pipeline stopping)/) {
            let id = Int(m.1) ?? 0
            sessions.removeAll { $0.id == id }
        } else if let m = line.firstMatch(of: /adb device connected: (\S+)/) {
            usbDevices.insert(String(m.1))
        } else if let m = line.firstMatch(of: /adb device disconnected: (\S+)/) {
            usbDevices.remove(String(m.1))
        } else if line.contains("address already in use") {
            error = "El puerto 9000 está ocupado: hay otro servidor corriendo."
        } else if line.contains("capture error") {
            error = "No se pudo capturar la pantalla virtual. Dale permiso de Grabación de pantalla, cerrá y volvé a abrir la app."
        } else if line.contains("virtual display error") || line.contains("encoder error") {
            error = "Falló el pipeline de video. Revisá el log."
        }
    }

    private func update(session id: Int, _ change: (inout PhoneSession) -> Void) {
        guard let index = sessions.firstIndex(where: { $0.id == id }) else { return }
        change(&sessions[index])
    }
}

// MARK: - UI

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }

    func applicationWillTerminate(_ notification: Notification) {
        MainActor.assumeIsolated { ServerController.shared.stop() }
    }
}

@main
struct AndroidDisplayApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        Window("Android Display", id: "main") {
            ContentView()
        }
        .windowResizability(.contentSize)
    }
}

struct ContentView: View {
    @ObservedObject private var server = ServerController.shared
    @ObservedObject private var settings = SettingsStore.shared
    @State private var accessibilityOK = AXIsProcessTrusted()
    @State private var screenCaptureOK = CGPreflightScreenCaptureAccess()

    private let permissionTimer = Timer.publish(every: 2, on: .main, in: .common).autoconnect()

    private var phones: [(id: String, phone: PhoneSettings)] {
        func isConnected(_ id: String) -> Bool { server.sessions.contains { $0.deviceId == id } }
        return settings.file.devices
            .map { (id: $0.key, phone: $0.value) }
            .sorted { a, b in
                let (ca, cb) = (isConnected(a.id), isConnected(b.id))
                return ca != cb ? ca : a.phone.name.localizedCompare(b.phone.name) == .orderedAscending
            }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            status

            Divider()

            defaultsSection

            if !phones.isEmpty {
                phonesSection
            }

            HStack {
                if server.state == .stopped {
                    Button("Iniciar") { server.start() }
                        .keyboardShortcut(.defaultAction)
                } else {
                    Button("Detener") { server.stop() }
                }
                Spacer()
                Button("Ver log") { NSWorkspace.shared.open(server.logURL) }
            }

            if !accessibilityOK || !screenCaptureOK {
                Divider()
                permissions
            }
        }
        .padding(20)
        .frame(width: 520)
        .onAppear { server.start() }
        .onReceive(permissionTimer) { _ in
            accessibilityOK = AXIsProcessTrusted()
            screenCaptureOK = CGPreflightScreenCaptureAccess()
        }
    }

    // MARK: Status

    private var status: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 8) {
                Circle().fill(statusColor).frame(width: 10, height: 10)
                Text(statusTitle).font(.headline)
            }
            if server.state == .running, !server.usbDevices.isEmpty {
                let n = server.usbDevices.count
                Text("USB: \(n) detectado\(n == 1 ? "" : "s")").font(.callout).foregroundStyle(.secondary)
            }
            if let error = server.error {
                Text(error).font(.callout).foregroundStyle(.red)
            }
        }
    }

    private var statusColor: Color {
        if server.state == .stopped { return .gray }
        return server.sessions.contains(where: \.isStreaming) ? .green : .orange
    }

    private var statusTitle: String {
        guard server.state == .running else { return "Servidor detenido" }
        switch server.sessions.count {
        case 0: return "Esperando celulares…"
        case 1: return "1 celular conectado"
        default: return "\(server.sessions.count) celulares conectados"
        }
    }

    // MARK: Defaults

    private var defaultsSection: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Predeterminado").font(.headline)
            Text("Se usa en los celulares que no tienen ajustes propios.")
                .font(.callout).foregroundStyle(.secondary)
            Form {
                Picker("Resolución", selection: Binding(
                    get: { settings.defaults.scale },
                    set: { settings.setDefaults(scale: $0) }
                )) {
                    ForEach(scaleOptions, id: \.self) { s in
                        Text(scaleLabel(s, native: server.nativeSize)).tag(s)
                    }
                }
                Picker("Calidad", selection: Binding(
                    get: { settings.defaults.bitrate },
                    set: { settings.setDefaults(bitrate: $0) }
                )) {
                    ForEach(bitrateOptions, id: \.self) { Text(bitrateLabel($0)).tag($0) }
                }
                Picker("Toque", selection: Binding(
                    get: { settings.defaults.touchMode ?? "cursor" },
                    set: { settings.setDefaults(touchMode: $0) }
                )) {
                    Text("Mueve el cursor (compatible con todo)").tag("cursor")
                    Text("Devuelve el cursor al terminar").tag("restore")
                    Text("Sin mover el cursor (experimental)").tag("background")
                }
            }
            if (settings.defaults.touchMode ?? "cursor") == "background" {
                Text("Cada toque va solo a la app que está bajo el dedo. El menú, el Dock y el escritorio igual mueven el cursor, y algunas apps (Chrome, juegos) pueden ignorar el toque.")
                    .font(.callout).foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
            Text("El modo de toque se aplica la próxima vez que el celular se conecta.")
                .font(.callout).foregroundStyle(.secondary)
        }
    }

    // MARK: Phones

    private var phonesSection: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("Celulares").font(.headline)
            Text("Cada celular puede tener su propia resolución y calidad. Se aplican la próxima vez que ese celular se conecta.")
                .font(.callout).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            ForEach(phones, id: \.id) { entry in
                PhoneRow(id: entry.id, phone: entry.phone, session: server.sessions.first { $0.deviceId == entry.id })
            }
        }
    }

    private func scaleLabel(_ s: Double, native: (width: Int, height: Int)?) -> String {
        guard let native else { return scalePercent(s) }
        return "\(scalePercent(s))  (\(scaledSize(native, s)))"
    }

    // MARK: Permissions

    private var permissions: some View {
        VStack(alignment: .leading, spacing: 8) {
            if !accessibilityOK {
                HStack {
                    Text("Falta permiso de Accesibilidad (toques del celular).").font(.callout)
                    Spacer()
                    Button("Abrir") {
                        openSettings("x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility")
                    }
                }
            }
            if !screenCaptureOK {
                HStack {
                    Text("Falta permiso de Grabación de pantalla.").font(.callout)
                    Spacer()
                    Button("Pedir") { CGRequestScreenCaptureAccess() }
                }
            }
        }
    }

    private func openSettings(_ url: String) {
        if let url = URL(string: url) { NSWorkspace.shared.open(url) }
    }
}

/// One phone's resolution and bitrate. "Predeterminada" clears the phone's own value.
struct PhoneRow: View {
    let id: String
    let phone: PhoneSettings
    let session: PhoneSession?
    @ObservedObject private var settings = SettingsStore.shared

    private var effectiveScale: Double { settings.scale(for: id) }
    private var effectiveBitrate: Int { settings.bitrate(for: id) }

    /// The running connection was made with other values than the ones now chosen.
    private var needsReconnect: Bool {
        guard let session, let scale = session.appliedScale, let bitrate = session.appliedBitrate else { return false }
        return abs(scale - effectiveScale) > 0.001 || bitrate != effectiveBitrate
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                Circle()
                    .fill(session == nil ? Color.gray : (session?.isStreaming == true ? Color.green : Color.orange))
                    .frame(width: 8, height: 8)
                Text(phone.name).font(.headline)
                if let size = session?.streamSize {
                    Text(size).foregroundStyle(.secondary)
                }
                Spacer()
                if session == nil {
                    Button("Quitar") { settings.forget(id) }.buttonStyle(.link)
                } else if let session {
                    if needsReconnect {
                        Button("Aplicar y reconectar") { ServerController.shared.reconnect(session: session.id) }
                            .buttonStyle(.borderedProminent)
                    } else {
                        Button("Reconectar") { ServerController.shared.reconnect(session: session.id) }
                    }
                }
            }

            HStack {
                Picker("Resolución", selection: Binding(
                    get: { phone.scale },
                    set: { settings.setScale($0, for: id) }
                )) {
                    Text("Predeterminada (\(scalePercent(settings.defaults.scale)))").tag(Double?.none)
                    ForEach(scaleOptions, id: \.self) { s in
                        Text(label(for: s)).tag(Double?.some(s))
                    }
                }
                Picker("Calidad", selection: Binding(
                    get: { phone.bitrate },
                    set: { settings.setBitrate($0, for: id) }
                )) {
                    Text("Predeterminada (\(bitrateLabel(settings.defaults.bitrate)))").tag(Int?.none)
                    ForEach(bitrateOptions, id: \.self) { Text(bitrateLabel($0)).tag(Int?.some($0)) }
                }
            }

            if needsReconnect {
                Text("Hay cambios sin aplicar: tocá Aplicar y reconectar. El celular se reconecta solo.")
                    .font(.callout)
                    .foregroundStyle(.orange)
            }
        }
        .padding(10)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.05)))
    }

    private func label(for s: Double) -> String {
        guard let native = phone.nativeSize else { return scalePercent(s) }
        return "\(scalePercent(s))  (\(scaledSize(native, s)))"
    }
}
