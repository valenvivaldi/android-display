package com.androidmac.client.video

import android.media.MediaCodec
import android.media.MediaFormat
import android.os.Build
import android.os.Handler
import android.os.HandlerThread
import android.util.Log
import android.view.Surface
import java.util.concurrent.ConcurrentLinkedQueue

/**
 * Holds a NAL unit together with its frame type and presentation timestamp
 * so the decoder can set the correct MediaCodec flags.
 */
data class NALEntry(val data: ByteArray, val frameType: Byte, val timestamp: Long)

/**
 * H.264 video decoder using MediaCodec async callback mode.
 *
 * Async callback eliminates the 10ms polling loop used in synchronous mode,
 * reducing decode latency by 10–20ms per frame. Input buffers are fed
 * immediately when both a NAL unit and a codec buffer are available.
 */
class VideoDecoder(
    private val width: Int,
    private val height: Int,
    private val surface: Surface
) {
    private var codec: MediaCodec? = null
    private var callbackThread: HandlerThread? = null

    // Thread-safe queues for matching NALs with codec input buffers.
    // When a NAL arrives but no buffer is free → NAL waits in nalQueue.
    // When a buffer is free but no NAL is ready → index waits in bufferQueue.
    private val nalQueue = ConcurrentLinkedQueue<NALEntry>()
    private val bufferQueue = ConcurrentLinkedQueue<Int>()

    // Synchronizes the check-and-act between nalQueue and bufferQueue
    // to prevent the race where both queues hold items that never meet.
    private val feedLock = Any()

    companion object {
        private const val TAG = "VideoDecoder"
        private const val MIME = "video/avc"

        // Frame type constants matching the Go stream package
        const val FRAME_TYPE_SPS: Byte = 0x10
        const val FRAME_TYPE_PPS: Byte = 0x11
        const val FRAME_TYPE_IDR: Byte = 0x01

        // Max NAL queue depth before dropping P-frames.
        // At 60fps each queued NAL ≈ 16ms, so 5 items ≈ 80ms max decode lag.
        private const val MAX_QUEUE_DEPTH = 5
    }

    fun start() {
        val thread = HandlerThread("VideoDecoderCB").apply { start() }
        callbackThread = thread

        val format = MediaFormat.createVideoFormat(MIME, width, height)
        // Request low-latency decode if supported (API 30+)
        try {
            format.setInteger(MediaFormat.KEY_LOW_LATENCY, 1)
        } catch (_: Exception) {}
        // Prefer realtime priority
        try {
            format.setInteger(MediaFormat.KEY_PRIORITY, 0) // 0 = realtime
        } catch (_: Exception) {}
        // Qualcomm decoders (c2.qti.* / OMX.qcom.*) ignore KEY_LOW_LATENCY unless they declare the
        // low-latency feature, and otherwise hold ~8 frames (~130 ms at 60 fps) before output.
        // These vendor keys make them output each frame as soon as it is decoded; other decoders
        // ignore unknown keys.
        format.setInteger("vendor.qti-ext-dec-low-latency.enable", 1)
        format.setInteger("vendor.qti-ext-dec-picture-order.enable", 1)

        codec = MediaCodec.createDecoderByType(MIME).apply {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
                setCallback(codecCallback, Handler(thread.looper))
            } else {
                // setCallback(Callback, Handler) is API 23+. Before that the callbacks run on the
                // calling thread's looper (the main thread here); they only feed and release buffers.
                setCallback(codecCallback)
            }
            configure(format, surface, null, 0)
            start()
        }
        Log.d(TAG, "Decoder started (async): ${codec?.name} ${width}x${height}")
    }

    private val codecCallback = object : MediaCodec.Callback() {
        override fun onInputBufferAvailable(mc: MediaCodec, index: Int) {
            synchronized(feedLock) {
                val entry = nalQueue.poll()
                if (entry != null) {
                    feedBuffer(mc, index, entry)
                } else {
                    bufferQueue.offer(index)
                }
            }
        }

        override fun onOutputBufferAvailable(mc: MediaCodec, index: Int, info: MediaCodec.BufferInfo) {
            // presentationTimeUs carries the frame's capture time on the Mac (packet header).
            LatencyStats.onDecoded(info.presentationTimeUs)
            try {
                mc.releaseOutputBuffer(index, true)
            } catch (e: Exception) {
                Log.w(TAG, "releaseOutputBuffer: ${e.message}")
            }
        }

        override fun onError(mc: MediaCodec, e: MediaCodec.CodecException) {
            Log.e(TAG, "Codec error: ${e.message}")
        }

        override fun onOutputFormatChanged(mc: MediaCodec, format: MediaFormat) {
            Log.d(TAG, "Output format: $format")
        }
    }

    private fun feedBuffer(mc: MediaCodec, index: Int, entry: NALEntry) {
        try {
            val buf = mc.getInputBuffer(index) ?: return
            buf.clear()
            buf.put(entry.data)
            val flags = when (entry.frameType) {
                FRAME_TYPE_SPS, FRAME_TYPE_PPS -> MediaCodec.BUFFER_FLAG_CODEC_CONFIG
                else -> 0
            }
            mc.queueInputBuffer(index, 0, entry.data.size, entry.timestamp, flags)
        } catch (e: Exception) {
            Log.w(TAG, "feedBuffer: ${e.message}")
        }
    }

    /**
     * Submit a NAL unit for decoding. If an input buffer is already
     * waiting, the NAL is fed immediately (zero queue delay).
     *
     * When the NAL queue exceeds MAX_QUEUE_DEPTH, P-frames are dropped
     * to prevent unbounded latency buildup. SPS/PPS/IDR frames are
     * never dropped since they're required for decoder recovery.
     */
    fun submitNAL(data: ByteArray, frameType: Byte, timestamp: Long) {
        val mc = codec ?: return

        // Drop P-frames if decoder is falling behind to cap latency
        if (nalQueue.size > MAX_QUEUE_DEPTH &&
            frameType != FRAME_TYPE_SPS &&
            frameType != FRAME_TYPE_PPS &&
            frameType != FRAME_TYPE_IDR) {
            LatencyStats.onDecoderDrop()
            return
        }

        val entry = NALEntry(data, frameType, timestamp)
        synchronized(feedLock) {
            val idx = bufferQueue.poll()
            if (idx != null) {
                feedBuffer(mc, idx, entry)
            } else {
                nalQueue.offer(entry)
                LatencyStats.onQueueDepth(nalQueue.size)
            }
        }
    }

    fun stop() {
        try {
            codec?.stop()
            codec?.release()
        } catch (_: Exception) {}
        codec = null
        callbackThread?.quitSafely()
        callbackThread = null
        nalQueue.clear()
        bufferQueue.clear()
        Log.d(TAG, "Decoder stopped")
    }
}
