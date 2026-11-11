package com.androidmac.client.protocol

import org.json.JSONArray
import org.json.JSONObject
import java.nio.ByteBuffer

data class ScreenInfo(val width: Int, val height: Int, val dpi: Int)

data class ClientHello(
    val device: String,
    val screen: ScreenInfo,
    val capabilities: List<String>,
    val codecs: List<String>,
    val connectionType: String = "wifi",
    /** Stable per-phone id (Android ID). The Mac keys the display's identity and saved position on it. */
    val deviceId: String = ""
) {
    fun toJson(): String {
        val obj = JSONObject()
        obj.put("device", device)
        if (deviceId.isNotEmpty()) {
            obj.put("deviceId", deviceId)
        }
        obj.put("screen", JSONObject().apply {
            put("width", screen.width)
            put("height", screen.height)
            put("dpi", screen.dpi)
        })
        obj.put("capabilities", JSONArray(capabilities))
        obj.put("codecs", JSONArray(codecs))
        if (connectionType != "wifi") {
            obj.put("connectionType", connectionType)
        }
        return obj.toString()
    }
}

data class DisplayInfo(val width: Int, val height: Int)

data class ServerHello(
    /** Identifies this phone on the Mac; sent first on the touch and video connections (see [sessionHeader]). */
    val sessionId: Long,
    val virtualDisplay: DisplayInfo,
    val codec: String,
    val bitrate: Int,
    val fps: Int,
    val streamPort: Int,
    val touchPort: Int,
    val videoPort: Int = 0
) {
    companion object {
        fun fromJson(json: String): ServerHello {
            val obj = JSONObject(json)
            val disp = obj.getJSONObject("virtualDisplay")
            return ServerHello(
                sessionId = obj.optLong("sessionId", 0),
                virtualDisplay = DisplayInfo(disp.getInt("width"), disp.getInt("height")),
                codec = obj.getString("codec"),
                bitrate = obj.getInt("bitrate"),
                fps = obj.getInt("fps"),
                streamPort = obj.getInt("streamPort"),
                touchPort = obj.optInt("touchPort", 0),
                videoPort = obj.optInt("videoPort", 0)
            )
        }
    }
}

/** A message the Mac sends on the control channel after the handshake (one JSON object per line). */
object ServerMessage {
    /** The Mac asks this phone to disconnect and connect again, so new settings take effect. */
    const val RECONNECT = "reconnect"

    /** The message's type, or "" when the line is not a JSON object with a type. */
    fun typeOf(line: String): String = try {
        JSONObject(line).optString("type")
    } catch (_: Exception) {
        ""
    }
}

/** First 4 bytes (big-endian) on the touch and video connections; routes them to this phone's session. */
fun sessionHeader(sessionId: Long): ByteArray =
    ByteBuffer.allocate(4).putInt(sessionId.toInt()).array() // ByteBuffer is big-endian by default

data class ClientReady(val udpPort: Int) {
    fun toJson(): String {
        val obj = JSONObject()
        obj.put("udpPort", udpPort)
        return obj.toString()
    }
}
