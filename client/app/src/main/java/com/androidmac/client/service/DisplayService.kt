package com.androidmac.client.service

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.os.Binder
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.util.Log
import androidx.core.app.NotificationCompat
import com.androidmac.client.R
import com.androidmac.client.control.ControlClient
import com.androidmac.client.touch.TouchEvent
import com.androidmac.client.touch.TouchSender
import com.androidmac.client.video.LatencyStats
import com.androidmac.client.video.TcpVideoReceiver
import com.androidmac.client.video.UdpReceiver
import com.androidmac.client.video.VideoPacket
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.launch
import java.io.ByteArrayOutputStream
import java.io.IOException

/**
 * DisplayService 是前景服務，負責管理所有連線狀態（控制、視訊、觸控），
 * 並在通知列顯示連線品質和中斷按鈕。
 *
 * 生命週期：
 * 1. MainActivity 完成握手後啟動此服務
 * 2. 服務初始化 UDP 接收、觸控連線
 * 3. DisplayActivity 綁定服務，註冊影像回呼
 * 4. 用戶按中斷或連線斷開 → 服務清理並停止
 */
class DisplayService : Service() {

    companion object {
        private const val TAG = "DisplayService"
        private const val CHANNEL_ID = "display_service"
        private const val NOTIFICATION_ID = 1
        private const val ACTION_DISCONNECT = "com.androidmac.client.DISCONNECT"
        private const val STATS_UPDATE_INTERVAL = 2000L // 每 2 秒更新

        /**
         * MainActivity 握手成功後將 ControlClient 暫存於此，
         * 服務啟動後立即取走。
         */
        @Volatile
        var pendingControlClient: ControlClient? = null
    }

    // Binder for DisplayActivity
    inner class LocalBinder : Binder() {
        fun getService(): DisplayService = this@DisplayService
    }

    private val binder = LocalBinder()

    /** 影像封包回呼，由 DisplayActivity 註冊 */
    interface VideoCallback {
        fun onNAL(data: ByteArray, frameType: Byte, timestamp: Long)
    }

    /** 連線中斷回呼，由 DisplayActivity 註冊 */
    interface DisconnectCallback {
        fun onDisconnected()
    }

    // 連線狀態
    private var controlClient: ControlClient? = null
    private var udpReceiver: UdpReceiver? = null
    private var tcpVideoReceiver: TcpVideoReceiver? = null
    private var touchSender: TouchSender? = null
    private var receiveJob: Job? = null
    private var touchJob: Job? = null
    private val serviceScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    // Buffered channel for touch events — single consumer, no per-event coroutine overhead
    private val touchChannel = Channel<TouchEvent>(capacity = 64)

    // 回呼
    @Volatile
    var videoCallback: VideoCallback? = null
    @Volatile
    var disconnectCallback: DisconnectCallback? = null

    // 快取最近一組 SPS/PPS，供解碼器重啟時注入（例如鎖屏解鎖後）
    @Volatile
    var cachedSPS: ByteArray? = null
        private set
    @Volatile
    var cachedPPS: ByteArray? = null
        private set

    // 連線參數
    private var serverHost: String = ""
    private var touchPort: Int = 0
    private var videoPort: Int = 0
    private var videoWidth: Int = 0
    private var videoHeight: Int = 0
    private var connectionType: String = "wifi"

    // 分片重組狀態
    private var currentSequence: Long = -1
    private var currentFrameType: Byte = 0
    private var currentTimestamp: Long = 0
    private val fragments = mutableMapOf<Int, ByteArray>()
    private var expectedFragTotal: Int = 0

    // 統計
    private var packetCount: Long = 0
    private var submitCount: Long = 0
    private var dropCount: Long = 0
    private var startTime: Long = 0
    private var lastSubmitCount: Long = 0
    private var lastStatsTime: Long = 0

    // 通知更新
    private lateinit var notificationManager: NotificationManager
    private val statsHandler = Handler(Looper.getMainLooper())
    private val statsRunnable = object : Runnable {
        override fun run() {
            updateNotificationStats()
            LatencyStats.logAndReset()
            statsHandler.postDelayed(this, STATS_UPDATE_INTERVAL)
        }
    }

    // 中斷連線廣播接收器
    private val disconnectReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            Log.d(TAG, "Disconnect action received")
            disconnect()
        }
    }

    override fun onCreate() {
        super.onCreate()
        notificationManager = getSystemService(NotificationManager::class.java)
        createNotificationChannel()

        // 註冊中斷廣播
        registerReceiver(
            disconnectReceiver,
            IntentFilter(ACTION_DISCONNECT),
            RECEIVER_NOT_EXPORTED
        )
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        // 立即進入前景模式
        startForeground(NOTIFICATION_ID, buildNotification(getString(R.string.notification_connecting)))

        // 取得連線參數
        serverHost = intent?.getStringExtra("serverHost") ?: ""
        touchPort = intent?.getIntExtra("touchPort", 0) ?: 0
        videoPort = intent?.getIntExtra("videoPort", 0) ?: 0
        videoWidth = intent?.getIntExtra("width", 1920) ?: 1920
        videoHeight = intent?.getIntExtra("height", 1080) ?: 1080
        connectionType = intent?.getStringExtra("connectionType") ?: "wifi"
        Log.d(TAG, "Connection: type=$connectionType host=$serverHost touchPort=$touchPort videoPort=$videoPort")

        // 取得 ControlClient
        controlClient = pendingControlClient
        pendingControlClient = null

        if (controlClient == null) {
            Log.e(TAG, "No ControlClient available, stopping service")
            stopSelf()
            return START_NOT_STICKY
        }

        startTime = System.currentTimeMillis()
        lastStatsTime = startTime

        // 啟動連線管線
        startPipeline()

        return START_NOT_STICKY
    }

    override fun onBind(intent: Intent?): IBinder = binder

    override fun onDestroy() {
        super.onDestroy()
        statsHandler.removeCallbacks(statsRunnable)
        try {
            unregisterReceiver(disconnectReceiver)
        } catch (_: Exception) {}
        cleanupConnections()
        serviceScope.cancel()
        Log.d(TAG, "Service destroyed")
    }

    /**
     * 啟動視訊接收 + 觸控連線管線
     * 根據 connectionType 選擇 UDP（WiFi）或 TCP（USB）傳輸
     */
    private fun startPipeline() {
        serviceScope.launch {
            try {
                if (connectionType == "usb") {
                    startPipelineUSB()
                } else {
                    startPipelineWiFi()
                }

                // 啟動觸控連線
                startTouchConnection()

                // 監控控制連線
                monitorControlConnection()

                // 更新通知為已連線
                Handler(Looper.getMainLooper()).post {
                    val modeLabel = if (connectionType == "usb") "USB" else "WiFi"
                    updateNotification("${getString(R.string.notification_connected)} [$modeLabel]")
                    statsHandler.postDelayed(statsRunnable, STATS_UPDATE_INTERVAL)
                }

                Log.d(TAG, "Pipeline started successfully ($connectionType)")
            } catch (e: Exception) {
                Log.e(TAG, "Pipeline start failed: ${e.message}", e)
                Handler(Looper.getMainLooper()).post {
                    disconnect()
                }
            }
        }
    }

    /**
     * WiFi 模式：綁定 UDP socket → 發送 ClientReady（含 UDP port）→ 啟動 UDP 接收
     */
    private suspend fun startPipelineWiFi() {
        val receiver = UdpReceiver(0)
        val actualPort = receiver.bind()
        udpReceiver = receiver
        Log.d(TAG, "WiFi: UDP bound to port $actualPort")

        controlClient?.sendReady(actualPort)
        Log.d(TAG, "WiFi: Sent ClientReady with UDP port $actualPort")

        receiveJob = serviceScope.launch {
            receiver.receiveLoop { packet ->
                handlePacket(packet)
            }
        }
    }

    /**
     * USB 模式：發送 ClientReady（UDPPort=0）→ 連接 TCP 視訊 → 啟動 TCP 接收
     */
    private suspend fun startPipelineUSB() {
        // USB 模式下 UDPPort = 0，通知 server 使用 TCP 視訊
        controlClient?.sendReady(0)
        Log.d(TAG, "USB: Sent ClientReady (TCP mode)")

        val receiver = TcpVideoReceiver(serverHost, videoPort)
        receiver.connect()
        tcpVideoReceiver = receiver
        Log.d(TAG, "USB: TCP video connected to $serverHost:$videoPort")

        receiveJob = serviceScope.launch {
            receiver.receiveLoop { packet ->
                handlePacket(packet)
            }
        }
    }

    /**
     * 監控 TCP 控制連線是否斷開
     */
    private fun monitorControlConnection() {
        serviceScope.launch {
            try {
                val socket = controlClient?.getSocket() ?: return@launch
                val buf = ByteArray(1)
                while (true) {
                    val n = socket.getInputStream().read(buf)
                    if (n == -1) {
                        Log.d(TAG, "Control connection closed by server")
                        Handler(Looper.getMainLooper()).post { disconnect() }
                        return@launch
                    }
                }
            } catch (e: IOException) {
                Log.d(TAG, "Control connection lost: ${e.message}")
                Handler(Looper.getMainLooper()).post { disconnect() }
            }
        }
    }

    /**
     * 啟動觸控 TCP 連線，並啟動 Channel 消費者協程。
     * 單一協程消費所有觸控事件，避免 per-event launch 的排程開銷。
     */
    private fun startTouchConnection() {
        if (touchPort <= 0 || serverHost.isEmpty()) {
            Log.w(TAG, "Touch not available: port=$touchPort host=$serverHost")
            return
        }

        serviceScope.launch {
            try {
                val sender = TouchSender()
                sender.connect(serverHost, touchPort)
                touchSender = sender
                Log.d(TAG, "Touch connected to $serverHost:$touchPort")

                // Single consumer coroutine — drains touchChannel on IO thread
                touchJob = serviceScope.launch(Dispatchers.IO) {
                    for (event in touchChannel) {
                        try {
                            sender.sendDirect(event)
                        } catch (e: Exception) {
                            Log.e(TAG, "Touch send error: ${e.message}")
                            break
                        }
                    }
                }
            } catch (e: Exception) {
                Log.e(TAG, "Touch connection failed: ${e.message}", e)
            }
        }
    }

    /**
     * 發送觸控事件（由 DisplayActivity 呼叫）。
     * 使用 Channel.trySend 替代 launch，避免每事件創建協程的開銷。
     */
    fun sendTouchEvent(event: TouchEvent) {
        touchChannel.trySend(event)
    }

    /**
     * 處理收到的 UDP 封包，進行分片重組。
     * SPS/PPS 會被快取，供解碼器重啟時立即注入。
     */
    private fun handlePacket(packet: VideoPacket) {
        packetCount++

        if (packet.fragTotal <= 1) {
            // 快取 SPS/PPS 供解碼器重啟恢復
            when (packet.frameType) {
                0x10.toByte() -> cachedSPS = packet.payload.copyOf()
                0x11.toByte() -> cachedPPS = packet.payload.copyOf()
            }
            // 單分片 NAL，直接提交
            submitCount++
            if (isVideoFrame(packet.frameType)) LatencyStats.onReceived(packet.timestamp)
            videoCallback?.onNAL(packet.payload, packet.frameType, packet.timestamp)
            return
        }

        // 多分片重組
        synchronized(this) {
            if (packet.sequence != currentSequence) {
                // 新的 frame —— 如果上一個未完成則記錄丟棄
                if (currentSequence >= 0 && fragments.size < expectedFragTotal) {
                    dropCount++
                }
                currentSequence = packet.sequence
                currentFrameType = packet.frameType
                currentTimestamp = packet.timestamp
                fragments.clear()
                expectedFragTotal = packet.fragTotal
            }

            fragments[packet.fragIndex] = packet.payload

            if (fragments.size == expectedFragTotal) {
                // 所有分片到齊，重組
                val assembled = ByteArrayOutputStream()
                for (i in 0 until expectedFragTotal) {
                    val frag = fragments[i]
                    if (frag != null) {
                        assembled.write(frag)
                    } else {
                        dropCount++
                        fragments.clear()
                        return
                    }
                }
                val ft = currentFrameType
                val ts = currentTimestamp
                val assembledData = assembled.toByteArray()
                fragments.clear()
                submitCount++
                if (isVideoFrame(ft)) LatencyStats.onReceived(ts)
                videoCallback?.onNAL(assembledData, ft, ts)
            }
        }
    }

    /** IDR or P frame (SPS/PPS share the frame's timestamp and would double-count). */
    private fun isVideoFrame(frameType: Byte) = frameType == 0x01.toByte() || frameType == 0x02.toByte()

    /**
     * 取得目前的連線統計
     */
    fun getStats(): ConnectionStats {
        val now = System.currentTimeMillis()
        val elapsed = (now - startTime) / 1000L
        val total = submitCount + dropCount
        val lossRate = if (total > 0) dropCount.toFloat() / total.toFloat() else 0f

        // 計算即時 FPS
        val dt = (now - lastStatsTime) / 1000f
        val fps = if (dt > 0) (submitCount - lastSubmitCount).toFloat() / dt else 0f

        return ConnectionStats(
            fps = fps,
            packetLossRate = lossRate,
            totalPackets = packetCount,
            totalFrames = submitCount,
            droppedFrames = dropCount,
            uptimeSeconds = elapsed
        )
    }

    /**
     * 中斷連線並停止服務
     */
    fun disconnect() {
        Log.d(TAG, "Disconnecting...")
        disconnectCallback?.onDisconnected()
        cleanupConnections()
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    private fun cleanupConnections() {
        receiveJob?.cancel()
        receiveJob = null
        touchJob?.cancel()
        touchJob = null
        touchChannel.close()
        udpReceiver?.close()
        udpReceiver = null
        tcpVideoReceiver?.close()
        tcpVideoReceiver = null
        touchSender?.disconnect()
        touchSender = null
        controlClient?.disconnect()
        controlClient = null
        videoCallback = null
        disconnectCallback = null
    }

    // ——— 通知相關 ———

    private fun createNotificationChannel() {
        val channel = NotificationChannel(
            CHANNEL_ID,
            getString(R.string.notification_channel_name),
            NotificationManager.IMPORTANCE_LOW
        ).apply {
            description = getString(R.string.notification_channel_desc)
            setShowBadge(false)
        }
        notificationManager.createNotificationChannel(channel)
    }

    private fun buildNotification(contentText: String): Notification {
        val disconnectIntent = Intent(ACTION_DISCONNECT).apply {
            setPackage(packageName)
        }
        val disconnectPending = PendingIntent.getBroadcast(
            this, 0, disconnectIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        )

        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_display)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(contentText)
            .setOngoing(true)
            .setSilent(true)
            .setOnlyAlertOnce(true)
            .addAction(
                R.drawable.ic_disconnect,
                getString(R.string.notification_disconnect),
                disconnectPending
            )
            .build()
    }

    private fun updateNotification(contentText: String) {
        notificationManager.notify(NOTIFICATION_ID, buildNotification(contentText))
    }

    private fun updateNotificationStats() {
        val stats = getStats()
        lastSubmitCount = submitCount
        lastStatsTime = System.currentTimeMillis()

        val text = String.format(
            getString(R.string.notification_stats_format),
            stats.fps,
            stats.packetLossRate * 100f
        )
        updateNotification(text)
    }
}
