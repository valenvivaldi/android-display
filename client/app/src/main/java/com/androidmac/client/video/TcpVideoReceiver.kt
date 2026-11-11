package com.androidmac.client.video

import android.util.Log
import com.androidmac.client.protocol.sessionHeader
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.isActive
import kotlinx.coroutines.withContext
import java.io.BufferedInputStream
import java.io.DataInputStream
import java.net.Socket

/**
 * TCP 視訊接收器 — USB 模式下替代 UdpReceiver。
 *
 * 連線至 server 的 TCP video port，讀取 length-prefixed 封包：
 *   [4-byte length (big-endian)] [17-byte header] [payload]
 *
 * 由於 TCP 保證順序和完整性，不需要分片重組 —
 * 每個封包都是完整的 NAL unit（fragIndex=0, fragTotal=1）。
 */
class TcpVideoReceiver(private val host: String, private val port: Int, private val sessionId: Long) {

    companion object {
        private const val TAG = "TcpVideoReceiver"
    }

    private var socket: Socket? = null
    private var input: DataInputStream? = null

    /**
     * 建立 TCP 連線到 server video port，並先送出 session header，讓 server 把連線交給這支手機的 session。
     */
    suspend fun connect() = withContext(Dispatchers.IO) {
        val sock = Socket(host, port)
        sock.tcpNoDelay = true
        sock.receiveBufferSize = 4 * 1024 * 1024 // match server write buffer
        sock.getOutputStream().apply {
            write(sessionHeader(sessionId))
            flush()
        }
        socket = sock
        input = DataInputStream(BufferedInputStream(sock.getInputStream(), 256 * 1024))
        Log.d(TAG, "Connected to $host:$port (session $sessionId)")
    }

    /**
     * 持續接收 TCP 視訊封包，直到 coroutine 取消或連線斷開。
     */
    suspend fun receiveLoop(onPacket: (VideoPacket) -> Unit) = withContext(Dispatchers.IO) {
        val inp = input ?: throw IllegalStateException("Not connected. Call connect() first.")

        // Reusable read buffer — grown on demand, avoids per-packet allocation.
        // Most NAL units are < 64KB; keyframes may be larger.
        var readBuf = ByteArray(64 * 1024)

        while (isActive) {
            try {
                // 讀取 4-byte length prefix
                val totalLen = inp.readInt()
                if (totalLen <= 0 || totalLen > 2 * 1024 * 1024) {
                    Log.w(TAG, "Invalid packet length: $totalLen, skipping")
                    continue
                }

                // Grow buffer if needed (keeps the larger size for subsequent frames)
                if (readBuf.size < totalLen) {
                    readBuf = ByteArray(totalLen)
                }

                // 讀取完整封包（header + payload）into reusable buffer
                inp.readFully(readBuf, 0, totalLen)

                // 解析封包
                val packet = PacketParser.parse(readBuf, 0, totalLen)
                onPacket(packet)
            } catch (e: Exception) {
                if (isActive) {
                    Log.e(TAG, "Receive error: ${e.message}")
                }
                break
            }
        }
    }

    fun close() {
        try {
            socket?.close()
        } catch (_: Exception) {}
        socket = null
        input = null
    }
}
