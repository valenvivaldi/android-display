package com.androidmac.client.control

import com.androidmac.client.protocol.ClientHello
import com.androidmac.client.protocol.ClientReady
import com.androidmac.client.protocol.ScreenInfo
import com.androidmac.client.protocol.ServerHello
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.BufferedReader
import java.io.InputStreamReader
import java.io.PrintWriter
import java.net.Socket

class ControlClient {
    private var socket: Socket? = null
    private var writer: PrintWriter? = null
    private var reader: BufferedReader? = null

    /**
     * Connect to the server and perform the handshake.
     * @param width  Native screen width; the server picks the streamed size
     * @param height Native screen height
     * @param dpi    Display DPI
     * @param connectionType "wifi" or "usb"
     * @param deviceId stable per-phone id; keys the display identity and saved position on the Mac
     */
    suspend fun connect(
        host: String,
        port: Int,
        width: Int,
        height: Int,
        dpi: Int,
        connectionType: String = "wifi",
        deviceId: String = ""
    ): ServerHello = withContext(Dispatchers.IO) {
        val sock = Socket(host, port)
        sock.tcpNoDelay = true
        socket = sock

        val w = PrintWriter(sock.getOutputStream(), true)
        val r = BufferedReader(InputStreamReader(sock.getInputStream()))
        writer = w
        reader = r

        val hello = ClientHello(
            device = android.os.Build.MODEL,
            screen = ScreenInfo(width, height, dpi),
            capabilities = listOf("touch", "pen", "pressure"),
            codecs = listOf("h264"),
            connectionType = connectionType,
            deviceId = deviceId
        )
        w.println(hello.toJson())

        val response = r.readLine() ?: throw Exception("Server closed connection")
        ServerHello.fromJson(response)
    }

    /**
     * Send ClientReady message to the server with the actual UDP port.
     * For USB mode, pass udpPort=0 (video uses TCP instead).
     */
    suspend fun sendReady(udpPort: Int) = withContext(Dispatchers.IO) {
        val w = writer ?: throw IllegalStateException("Not connected")
        w.println(ClientReady(udpPort).toJson())
    }

    /**
     * Blocks until the server sends its next control message (one line); null once the server
     * has closed the connection. Call it from a background thread.
     */
    fun readMessage(): String? = reader?.readLine()

    fun getSocket(): Socket? = socket

    fun disconnect() {
        try {
            socket?.close()
        } catch (_: Exception) {
        }
        socket = null
        writer = null
        reader = null
    }
}
