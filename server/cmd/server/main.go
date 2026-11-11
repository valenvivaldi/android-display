package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/luke/android-mac/server/internal/adb"
	"github.com/luke/android-mac/server/internal/capture"
	"github.com/luke/android-mac/server/internal/control"
	"github.com/luke/android-mac/server/internal/discovery"
	"github.com/luke/android-mac/server/internal/display"
	"github.com/luke/android-mac/server/internal/encoder"
	"github.com/luke/android-mac/server/internal/h264"
	"github.com/luke/android-mac/server/internal/input"
	"github.com/luke/android-mac/server/internal/stream"
	"github.com/luke/android-mac/server/internal/touch"
)

const (
	controlPort    = 9000
	defaultFPS     = 60
	defaultBitrate = 8_000_000

	// adbPollInterval is how often attached phones are checked for USB forwarding.
	adbPollInterval = 2 * time.Second
	// videoConnectTimeout is how long a USB phone has to open its video connection.
	videoConnectTimeout = 10 * time.Second
)

// server holds what every session shares. Each phone gets its own session on top of it.
type server struct {
	touch   *touch.Server
	video   *stream.TCPVideoServer
	origins *originStore
	// settings holds the Mac app's per-phone stream settings and the touch mode; it is
	// re-read on every use, so changes apply to the next connection.
	settings *settingsStore

	mu      sync.Mutex
	clients map[uint32]control.ClientConn // live sessions by id, for commands from the Mac app
}

func main() {
	scale := flag.Float64("scale", 1.0, "streamed resolution as a fraction of the device's native screen (0.25-1)")
	bitrate := flag.Int("bitrate", defaultBitrate, "video bitrate in bits per second, per phone")
	exitOnStdinEOF := flag.Bool("exit-on-stdin-eof", false, "shut down when stdin closes (set by the Mac app so the server dies with it)")
	flag.Parse()
	if *scale < 0.25 || *scale > 1 {
		log.Fatalf("-scale must be between 0.25 and 1, got %v", *scale)
	}
	if *bitrate <= 0 {
		log.Fatalf("-bitrate must be positive, got %d", *bitrate)
	}

	log.Println("android-mac server starting...")

	// 1. Touch server shared by all phones (auto-assign port); events are routed by session
	touchServer, err := touch.NewServer(0)
	if err != nil {
		log.Fatalf("touch server failed: %v", err)
	}
	defer touchServer.Stop()
	go touchServer.AcceptLoop()
	log.Printf("touch server on port %d", touchServer.Port())

	// 2. TCP video server for USB mode, shared by all phones (auto-assign port)
	videoServer, err := stream.NewTCPVideoServer(0)
	if err != nil {
		log.Fatalf("TCP video server failed: %v", err)
	}
	defer videoServer.Close()
	go videoServer.AcceptLoop()
	log.Printf("TCP video server on port %d", videoServer.Port())

	origins, err := defaultOriginStore()
	if err != nil {
		log.Fatalf("display position store: %v", err)
	}
	// Resolution and bitrate come from the Mac app's per-phone settings file;
	// -scale and -bitrate are what a phone gets when the file says nothing about it.
	settings, err := defaultSettingsStore(*scale, *bitrate)
	if err != nil {
		log.Fatalf("device settings: %v", err)
	}
	srv := &server{touch: touchServer, video: videoServer, origins: origins, settings: settings}

	// 3. Forward the ports to every USB phone, now and whenever one is plugged in later
	adbCtx, stopADB := context.WithCancel(context.Background())
	adbDone := make(chan struct{})
	adbManager, err := adb.NewManager()
	if err != nil {
		log.Printf("ADB not available: %v (USB connections disabled)", err)
		close(adbDone)
	} else {
		ports := []int{controlPort, touchServer.Port(), videoServer.Port()}
		go func() {
			defer close(adbDone)
			adbManager.Watch(adbCtx, ports, adbPollInterval)
		}()
	}

	// 4. Start mDNS advertisement
	hostname, _ := os.Hostname()
	mdns, err := discovery.NewService(hostname, controlPort)
	if err != nil {
		log.Fatalf("mDNS failed: %v", err)
	}
	defer mdns.Stop()
	log.Printf("mDNS advertising on port %d", controlPort)

	// 5. Start TCP control server
	ctrlServer, err := control.NewServer(controlPort, *scale, *bitrate)
	if err != nil {
		log.Fatalf("control server failed: %v", err)
	}
	defer ctrlServer.Stop()
	ctrlServer.SetTouchPort(touchServer.Port())
	ctrlServer.SetVideoPort(videoServer.Port())

	ctrlServer.SetStreamSettings(settings.lookup)

	// 6. Every client that connects gets its own capture-encode-stream pipeline
	ctrlServer.OnClient(srv.startSession)

	go ctrlServer.AcceptLoop()
	log.Printf("control server on port %d — waiting for connections...", controlPort)

	// 7. Wait for SIGINT/SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	if *exitOnStdinEOF {
		// The Mac app owns our stdin: it sends commands as lines, and closing it means "exit".
		go func() {
			srv.runCommands(os.Stdin)
			sig <- syscall.SIGTERM
		}()
	}
	<-sig
	log.Println("shutting down...")

	// Remove the ADB reverse forwarding from every phone
	stopADB()
	select {
	case <-adbDone:
	case <-time.After(5 * time.Second):
		log.Println("timed out removing ADB forwarding")
	}
}

// startSession runs one phone's pipeline until its control connection closes.
func (srv *server) startSession(client control.ClientConn) {
	s := &session{id: client.SessionID, deviceID: client.Hello.DeviceID, name: client.Hello.Device}
	if s.deviceID == "" {
		// Older clients don't send an id; the model is the best stable key left.
		s.deviceID = client.Hello.Device
	}

	ctx, cancel := context.WithCancel(context.Background())
	srv.track(s.id, client)
	go func() {
		<-ctx.Done()
		srv.untrack(s.id)
	}()

	// Monitor the TCP control connection
	go func() {
		buf := make([]byte, 1)
		for {
			_, err := client.Conn.Read(buf)
			if err != nil {
				if err != io.EOF {
					s.logf("control connection read error: %v", err)
				}
				s.logf("client disconnected, cancelling pipeline")
				cancel()
				return
			}
		}
	}()

	// Determine video transport based on connection type
	if client.Hello.IsUSB() {
		go srv.runUSB(ctx, cancel, s, client)
	} else {
		remoteAddr := client.Conn.RemoteAddr().String()
		host, _, _ := net.SplitHostPort(remoteAddr)
		targetAddr := net.JoinHostPort(host, strconv.Itoa(client.UDPPort))
		go srv.runWiFi(ctx, cancel, s, client, targetAddr)
	}
}

// runWiFi starts the capture-encode-stream pipeline using UDP transport.
func (srv *server) runWiFi(ctx context.Context, cancel context.CancelFunc, s *session, client control.ClientConn, targetAddr string) {
	defer cancel()
	s.logf("starting WiFi pipeline: %dx%d @ %d bps → %s", client.Width, client.Height, client.Bitrate, targetAddr)

	udpStreamer, err := stream.NewUDPStreamer(targetAddr)
	if err != nil {
		s.logf("UDP streamer error: %v", err)
		return
	}
	defer udpStreamer.Close()

	srv.runPipeline(ctx, s, client, udpStreamer)
}

// runUSB starts the capture-encode-stream pipeline using TCP transport.
func (srv *server) runUSB(ctx context.Context, cancel context.CancelFunc, s *session, client control.ClientConn) {
	defer cancel()
	s.logf("starting USB pipeline: %dx%d @ %d bps (TCP video)", client.Width, client.Height, client.Bitrate)

	// Wait for this session's phone to connect to the video port
	videoStream, err := srv.video.Accept(ctx, s.id, videoConnectTimeout)
	if err != nil {
		s.logf("TCP video accept error: %v", err)
		return
	}
	defer videoStream.Close()

	srv.runPipeline(ctx, s, client, videoStream)
}

// runPipeline contains the shared pipeline logic for both WiFi and USB modes.
func (srv *server) runPipeline(ctx context.Context, s *session, client control.ClientConn, videoStreamer stream.Streamer) {
	width, height, bitrate := client.Width, client.Height, client.Bitrate

	// Virtual display: its serial identifies this phone to macOS
	vd, err := display.New(display.Config{
		Width:  width,
		Height: height,
		PPI:    224,
		Serial: displaySerial(s.deviceID),
		Name:   s.name,
	})
	if err != nil {
		s.logf("virtual display error: %v", err)
		return
	}
	defer vd.Close()
	s.logf("virtual display created: 0x%x (%dx%d)", vd.DisplayID(), width, height)

	// macOS places a new virtual display at a default spot; put it back where the
	// user last arranged this phone's display. The position is saved whenever it
	// changes (polled, since a SIGTERM restart exits without running defers).
	if x, y, ok := srv.origins.load(s.deviceID); ok {
		if err := vd.SetOrigin(x, y); err != nil {
			s.logf("restore display position: %v", err)
		} else {
			s.logf("display position restored: (%d,%d)", x, y)
		}
	}
	stopTracking := make(chan struct{})
	trackingDone := make(chan struct{})
	go srv.trackDisplayOrigin(s, vd, stopTracking, trackingDone)
	// Registered after vd.Close, so it runs first: tracking stops before the display is destroyed.
	defer func() {
		close(stopTracking)
		<-trackingDone
	}()

	// Input injector + gesture recognizer for this display
	injector := input.NewInjector(vd.DisplayID(), srv.settings.touchMode())
	gesture := input.NewGestureRecognizer(func(me input.MouseEvent) {
		switch me.Action {
		case input.ActionMouseMove:
			injector.MouseMove(me.X, me.Y)
		case input.ActionLeftDown:
			injector.LeftMouseDown(me.X, me.Y)
		case input.ActionLeftUp:
			injector.LeftMouseUp(me.X, me.Y)
		case input.ActionLeftDragged:
			injector.LeftMouseDragged(me.X, me.Y)
		case input.ActionRightDown:
			injector.RightMouseDown(me.X, me.Y)
		case input.ActionRightUp:
			injector.RightMouseUp(me.X, me.Y)
		case input.ActionScroll:
			injector.ScrollWheel(me.ScrollX, me.ScrollY)
		}
	})
	defer gesture.Close()

	// Wire this session's touch events → pen events bypass gesture recognizer
	srv.touch.Register(s.id, func(e touch.Event) {
		if e.Type == touch.TouchTypePen {
			switch e.Action {
			case touch.TouchActionDown:
				injector.TabletProximityEnter()
				injector.TabletDown(e.X, e.Y, e.Pressure, e.TiltX, e.TiltY)
			case touch.TouchActionMove:
				injector.TabletDragged(e.X, e.Y, e.Pressure, e.TiltX, e.TiltY)
			case touch.TouchActionUp:
				injector.TabletUp(e.X, e.Y, e.Pressure, e.TiltX, e.TiltY)
				injector.TabletProximityLeave()
			}
		} else {
			gesture.HandleEvent(e)
		}
	})
	defer srv.touch.Unregister(s.id)
	s.logf("touch input enabled")

	stats := newPipelineStats(s.id)
	go stats.run(ctx, 2*time.Second)

	// H.264 encoder
	enc, err := encoder.New(encoder.Config{
		Width:   width,
		Height:  height,
		FPS:     defaultFPS,
		Bitrate: bitrate,
	}, func(nal encoder.NALUnit) {
		var ft byte
		data := nal.Data
		switch {
		case nal.NALType == 7:
			ft = stream.FrameTypeSPS
			// Tell decoders they never need to hold frames back; see h264.LimitDecoderBuffering.
			data = h264.LimitDecoderBuffering(data)
		case nal.NALType == 8:
			ft = stream.FrameTypePPS
		case nal.IsKeyframe:
			ft = stream.FrameTypeIDR
		default:
			ft = stream.FrameTypeP
		}
		// nal.Timestamp is the capture time on the mach clock; translate it to
		// Unix time for the packet header so the phone can measure latency.
		sinceCapture := time.Duration(capture.NowMicros()-nal.Timestamp) * time.Microsecond
		captureUnix := time.Now().Add(-sinceCapture).UnixMicro()
		sendStart := time.Now()
		if err := videoStreamer.SendFrame(data, ft, uint64(captureUnix)); err != nil {
			s.logf("stream send error: %v", err)
		}
		stats.recordBytes(len(data))
		if ft == stream.FrameTypeIDR || ft == stream.FrameTypeP {
			stats.recordFrame(sinceCapture, time.Since(sendStart))
		}
	})
	if err != nil {
		s.logf("encoder error: %v", err)
		return
	}
	defer enc.Close()

	// Re-send the last frame shortly after each change, so decoders that hold the newest
	// frame until the next one arrives still show it (see frameFlusher). It owns the pixel
	// buffers it is given. Deferred before cap.Stop and after enc.Close, so on the way out
	// capture stops first, then the flusher, then the encoder.
	flush := &frameFlusher{
		encode:  enc.Encode,
		release: capture.ReleasePixelBuffer,
		now:     capture.NowMicros,
		delay:   flushDelay,
		repeats: flushRepeats,
	}
	defer flush.stop()

	// Screen capture
	cap, err := capture.Start(vd.DisplayID(), defaultFPS, func(frame capture.Frame) {
		if err := enc.Encode(frame.PixelBuffer, frame.Timestamp); err != nil {
			s.logf("encode error: %v", err)
		}
		flush.hold(frame.PixelBuffer, frame.Timestamp)
	})
	if err != nil {
		s.logf("capture error: %v", err)
		return
	}
	defer cap.Stop()

	s.logf("pipeline active (video + touch)")
	<-ctx.Done()
	s.logf("pipeline stopping...")
}

// trackDisplayOrigin saves the display's position whenever the user moves it,
// and once more when stop is closed.
func (srv *server) trackDisplayOrigin(s *session, vd *display.VirtualDisplay, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	lastX, lastY := vd.Origin()
	save := func() {
		x, y := vd.Origin()
		if x == lastX && y == lastY {
			return
		}
		if err := srv.origins.save(s.deviceID, x, y); err != nil {
			s.logf("save display position: %v", err)
			return
		}
		lastX, lastY = x, y
		s.logf("display position saved: (%d,%d)", x, y)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			save()
			return
		case <-ticker.C:
			save()
		}
	}
}
