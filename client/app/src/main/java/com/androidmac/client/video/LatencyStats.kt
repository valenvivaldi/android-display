package com.androidmac.client.video

import android.util.Log

/**
 * Collects per-frame latencies on the phone and logs a summary to logcat (tag "Latency"):
 *
 *   recv avg 12.3 p95 20.1 ms | decoded avg 31.0 p95 45.2 ms | 58 fps | decoder dropped 0 | queue max 1
 *
 * Both latencies are measured from the frame's capture time on the Mac (packet header timestamp, Unix µs)
 * to the phone's wall clock, so they include the Mac↔phone clock offset. Subtract it to get real values
 * (e.g. measure it with `adb shell date +%s%N` against the Mac's clock).
 *  - recv: capture → complete NAL received.
 *  - decoded: capture → decoded frame handed to the display (it is shown at the next vsync, ~0–16 ms later).
 */
object LatencyStats {
    private const val TAG = "Latency"

    private val lock = Any()
    private var recv = LongArray(256)
    private var recvCount = 0
    private var decoded = LongArray(256)
    private var decodedCount = 0
    private var decoderDropped = 0
    private var maxQueue = 0
    private var windowStart = System.nanoTime()

    private fun nowMicros(): Long = System.currentTimeMillis() * 1000

    fun onReceived(captureMicros: Long) {
        val v = nowMicros() - captureMicros
        synchronized(lock) {
            if (recvCount == recv.size) recv = recv.copyOf(recv.size * 2)
            recv[recvCount++] = v
        }
    }

    fun onDecoded(captureMicros: Long) {
        val v = nowMicros() - captureMicros
        synchronized(lock) {
            if (decodedCount == decoded.size) decoded = decoded.copyOf(decoded.size * 2)
            decoded[decodedCount++] = v
        }
    }

    fun onDecoderDrop() {
        synchronized(lock) { decoderDropped++ }
    }

    fun onQueueDepth(depth: Int) {
        synchronized(lock) { if (depth > maxQueue) maxQueue = depth }
    }

    /** Logs the summary for the window since the last call and starts a new one. */
    fun logAndReset() {
        val line: String
        synchronized(lock) {
            val seconds = (System.nanoTime() - windowStart) / 1e9
            if (decodedCount == 0 && recvCount == 0) {
                windowStart = System.nanoTime()
                return
            }
            val (recvAvg, recvP95) = avgP95(recv, recvCount)
            val (decAvg, decP95) = avgP95(decoded, decodedCount)
            line = String.format(
                java.util.Locale.ROOT,
                "recv avg %.1f p95 %.1f ms | decoded avg %.1f p95 %.1f ms | %.0f fps | decoder dropped %d | queue max %d",
                recvAvg, recvP95, decAvg, decP95, decodedCount / seconds, decoderDropped, maxQueue
            )
            recvCount = 0
            decodedCount = 0
            decoderDropped = 0
            maxQueue = 0
            windowStart = System.nanoTime()
        }
        Log.i(TAG, line)
    }

    private fun avgP95(values: LongArray, count: Int): Pair<Double, Double> {
        if (count == 0) return 0.0 to 0.0
        val sorted = values.copyOf(count).also { it.sort() }
        return sorted.average() / 1000.0 to sorted[(count * 95) / 100] / 1000.0
    }
}
