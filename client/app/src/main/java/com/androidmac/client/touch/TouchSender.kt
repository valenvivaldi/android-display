package com.androidmac.client.touch

import com.androidmac.client.protocol.sessionHeader
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.OutputStream
import java.net.Socket

class TouchSender {
    private var socket: Socket? = null
    private var output: OutputStream? = null

    /** Connects and identifies this phone's session so the Mac routes the events to its display. */
    suspend fun connect(host: String, port: Int, sessionId: Long) = withContext(Dispatchers.IO) {
        socket = Socket(host, port).also {
            it.tcpNoDelay = true
            // Direct output stream — no BufferedOutputStream since we flush every write.
            output = it.getOutputStream().also { out ->
                out.write(sessionHeader(sessionId))
                out.flush()
            }
        }
    }

    /**
     * Write a touch event directly to the socket.
     * Called from a single-threaded Channel consumer, so no concurrency issues.
     */
    fun sendDirect(event: TouchEvent) {
        output?.let {
            it.write(event.toBytes())
            it.flush()
        }
    }

    fun disconnect() {
        try { socket?.close() } catch (_: Exception) {}
        socket = null
        output = null
    }
}
