package com.androidmac.client

import android.annotation.SuppressLint
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.os.Bundle
import android.os.IBinder
import android.util.Log
import android.view.MotionEvent
import android.view.SurfaceHolder
import android.view.SurfaceView
import android.view.View
import android.view.WindowInsets
import android.view.WindowInsetsController
import android.view.WindowManager
import androidx.appcompat.app.AppCompatActivity
import com.androidmac.client.service.DisplayService
import com.androidmac.client.touch.TouchEvent
import com.androidmac.client.video.VideoDecoder
import kotlin.math.cos
import kotlin.math.sin

/**
 * DisplayActivity 負責視訊解碼與畫面顯示。
 * 所有連線狀態由 DisplayService 管理，Activity 透過綁定 Service 接收 NAL 資料。
 */
class DisplayActivity : AppCompatActivity() {

    companion object {
        private const val TAG = "DisplayActivity"
    }

    private lateinit var surfaceView: SurfaceView
    private var videoDecoder: VideoDecoder? = null

    private var videoWidth: Int = 0
    private var videoHeight: Int = 0

    // Service 綁定
    private var displayService: DisplayService? = null
    private var serviceBound = false

    private val serviceConnection = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName?, binder: IBinder?) {
            val service = (binder as DisplayService.LocalBinder).getService()
            displayService = service
            serviceBound = true
            Log.d(TAG, "Bound to DisplayService")

            // 註冊影像回呼
            service.videoCallback = object : DisplayService.VideoCallback {
                override fun onNAL(data: ByteArray, frameType: Byte, timestamp: Long) {
                    videoDecoder?.submitNAL(data, frameType, timestamp)
                }
            }

            // 註冊斷線回呼
            service.disconnectCallback = object : DisplayService.DisconnectCallback {
                override fun onDisconnected() {
                    runOnUiThread { finish() }
                }
            }

            // The Mac asked this phone to reconnect (new settings): start a new connection
            // from the connect screen, which re-handshakes and picks up the new size and bitrate.
            service.reconnectCallback = object : DisplayService.ReconnectCallback {
                override fun onReconnect(host: String, connectionType: String) {
                    runOnUiThread {
                        startActivity(MainActivity.reconnectIntent(this@DisplayActivity, host, connectionType))
                        finish()
                    }
                }
            }

            // 設定觸控處理
            setupTouchHandling()
        }

        override fun onServiceDisconnected(name: ComponentName?) {
            displayService = null
            serviceBound = false
            Log.d(TAG, "Disconnected from DisplayService")
            finish()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_display)

        enterImmersiveMode()

        // Acting as a monitor: no touch for a while is normal, so keep the screen on
        // and at full brightness (without this Android dims it, then sleeps).
        window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)

        videoWidth = intent.getIntExtra("width", 1920)
        videoHeight = intent.getIntExtra("height", 1080)

        Log.d(TAG, "Display config: ${videoWidth}x${videoHeight}")

        surfaceView = findViewById(R.id.surfaceView)
        surfaceView.holder.addCallback(object : SurfaceHolder.Callback {
            override fun surfaceCreated(holder: SurfaceHolder) {
                startDecoder(holder)
            }

            override fun surfaceChanged(holder: SurfaceHolder, format: Int, width: Int, height: Int) {}

            override fun surfaceDestroyed(holder: SurfaceHolder) {
                stopDecoder()
            }
        })

        // 綁定 DisplayService
        val intent = Intent(this, DisplayService::class.java)
        bindService(intent, serviceConnection, Context.BIND_AUTO_CREATE)
    }

    private fun enterImmersiveMode() {
        window.insetsController?.let { controller ->
            controller.hide(WindowInsets.Type.systemBars())
            controller.systemBarsBehavior =
                WindowInsetsController.BEHAVIOR_SHOW_TRANSIENT_BARS_BY_SWIPE
        }
        @Suppress("DEPRECATION")
        window.decorView.systemUiVisibility = (
            View.SYSTEM_UI_FLAG_IMMERSIVE_STICKY
                or View.SYSTEM_UI_FLAG_FULLSCREEN
                or View.SYSTEM_UI_FLAG_HIDE_NAVIGATION
                or View.SYSTEM_UI_FLAG_LAYOUT_STABLE
                or View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN
                or View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION
            )
    }

    private fun startDecoder(holder: SurfaceHolder) {
        val decoder = VideoDecoder(videoWidth, videoHeight, holder.surface)
        decoder.start()
        videoDecoder = decoder
        Log.d(TAG, "Decoder started (async callback)")

        // 注入快取的 SPS/PPS，讓解碼器在下一個 IDR 到來時立即恢復畫面
        // （例如鎖屏解鎖後 Surface 重建的場景）
        displayService?.cachedSPS?.let { sps ->
            decoder.submitNAL(sps, VideoDecoder.FRAME_TYPE_SPS, 0)
            Log.d(TAG, "Injected cached SPS (${sps.size} bytes)")
        }
        displayService?.cachedPPS?.let { pps ->
            decoder.submitNAL(pps, VideoDecoder.FRAME_TYPE_PPS, 0)
            Log.d(TAG, "Injected cached PPS (${pps.size} bytes)")
        }
    }

    private fun stopDecoder() {
        videoDecoder?.stop()
        videoDecoder = null
        Log.d(TAG, "Decoder stopped")
    }

    @SuppressLint("ClickableViewAccessibility")
    private fun setupTouchHandling() {
        surfaceView.setOnTouchListener { view, event ->
            when (event.actionMasked) {
                MotionEvent.ACTION_DOWN, MotionEvent.ACTION_POINTER_DOWN -> {
                    val idx = event.actionIndex
                    sendTouchEvent(view, event, idx, TouchEvent.ACTION_DOWN)
                }
                MotionEvent.ACTION_MOVE -> {
                    for (i in 0 until event.pointerCount) {
                        sendTouchEvent(view, event, i, TouchEvent.ACTION_MOVE)
                    }
                }
                MotionEvent.ACTION_CANCEL -> {
                    for (i in 0 until event.pointerCount) {
                        sendTouchEvent(view, event, i, TouchEvent.ACTION_UP)
                    }
                }
                MotionEvent.ACTION_UP, MotionEvent.ACTION_POINTER_UP -> {
                    val idx = event.actionIndex
                    sendTouchEvent(view, event, idx, TouchEvent.ACTION_UP)
                }
            }
            true
        }
    }

    private fun sendTouchEvent(view: View, event: MotionEvent, index: Int, action: Byte) {
        val touchType = when (event.getToolType(index)) {
            MotionEvent.TOOL_TYPE_STYLUS -> TouchEvent.TYPE_PEN
            else -> TouchEvent.TYPE_FINGER
        }

        val (tiltX, tiltY) = if (event.getToolType(index) == MotionEvent.TOOL_TYPE_STYLUS) {
            val tilt = event.getAxisValue(MotionEvent.AXIS_TILT, index)
            val orientation = event.getAxisValue(MotionEvent.AXIS_ORIENTATION, index)
            Pair(
                (sin(orientation.toDouble()) * sin(tilt.toDouble())).toFloat(),
                (cos(orientation.toDouble()) * sin(tilt.toDouble())).toFloat()
            )
        } else {
            Pair(0f, 0f)
        }

        val touchEvent = TouchEvent(
            type = touchType,
            action = action,
            x = (event.getX(index) / view.width.toFloat()).coerceIn(0f, 1f),
            y = (event.getY(index) / view.height.toFloat()).coerceIn(0f, 1f),
            pressure = event.getPressure(index).coerceIn(0f, 1f),
            tiltX = tiltX,
            tiltY = tiltY,
            pointerId = event.getPointerId(index),
            timestamp = System.currentTimeMillis()
        )

        displayService?.sendTouchEvent(touchEvent)
    }

    override fun onDestroy() {
        super.onDestroy()
        stopDecoder()
        if (serviceBound) {
            // 清除回呼避免 leak
            displayService?.videoCallback = null
            displayService?.disconnectCallback = null
            displayService?.reconnectCallback = null
            unbindService(serviceConnection)
            serviceBound = false
        }
        Log.d(TAG, "DisplayActivity destroyed")
    }

    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (hasFocus) {
            enterImmersiveMode()
        }
    }
}
