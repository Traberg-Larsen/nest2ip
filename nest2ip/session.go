package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	cmdGenerate = "sdm.devices.commands.CameraLiveStream.GenerateWebRtcStream"
	cmdExtend   = "sdm.devices.commands.CameraLiveStream.ExtendWebRtcStream"
	cmdStop     = "sdm.devices.commands.CameraLiveStream.StopWebRtcStream"
)

var (
	errNoMedia  = errors.New("forbundet, men ingen video fra Google inden for 20 s")
	errStalled  = errors.New("videoen fra Google stoppede")
	errICE      = errors.New("WebRTC-forbindelsen til Google faldt")
	errNoAnswer = errors.New("Google gav intet svar-SDP")
)

var sessionSeq atomic.Uint64

// Session is one WebRTC session with Google for one camera.
type Session struct {
	ID        uint64
	MediaID   string
	ExpiresAt time.Time

	pc       *webrtc.PeerConnection
	out      *Output
	videoSSR atomic.Uint32
	firSeq   atomic.Uint32
	lastFIR  atomic.Int64

	lastRTP   atomic.Int64  // unix nanos of the last video RTP packet
	frames    atomic.Uint64 // access units written to RTSP
	connected chan struct{}
	firstRTP  chan struct{}
	dead      chan struct{}
	deadErr   error
	deadOnce  sync.Once
	connOnce  sync.Once
	firstOnce sync.Once
}

func newAPI() (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	fb := []webrtc.RTCPFeedback{{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"}}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	h264 := []struct {
		pt   webrtc.PayloadType
		fmtp string
	}{
		{102, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f"},
		{106, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		{112, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"},
		{114, "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f"},
	}
	for _, c := range h264 {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: c.fmtp, RTCPFeedback: fb},
			PayloadType:        c.pt,
		}, webrtc.RTPCodecTypeVideo); err != nil {
			return nil, err
		}
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	s := webrtc.SettingEngine{}
	s.SetDTLSInsecureSkipHelloVerify(true) // same as go2rtc: avoids DTLS handshake trouble (pion/webrtc#2407)
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6})
	s.SetIPFilter(func(ip net.IP) bool { return !dockerNet.Contains(ip) }) // skip HA's internal Docker networks
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(s)), nil
}

var _, dockerNet, _ = net.ParseCIDR("172.16.0.0/12")

// generateResult is the answer to GenerateWebRtcStream.
type generateResult struct {
	AnswerSdp      string `json:"answerSdp"`
	ExpiresAt      string `json:"expiresAt"`
	MediaSessionID string `json:"mediaSessionId"`
}

// openSession creates the offer, asks Google for a stream and waits for the first video packet.
// It costs exactly one Generate call (plus one best-effort Stop if it fails after Google said yes).
func (c *Camera) openSession(ctx context.Context) (*Session, error) {
	api, err := newAPI()
	if err != nil {
		return nil, err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:        sessionSeq.Add(1),
		pc:        pc,
		out:       c.out,
		connected: make(chan struct{}),
		firstRTP:  make(chan struct{}),
		dead:      make(chan struct{}),
	}
	ok := false
	defer func() {
		if !ok {
			_ = pc.Close()
		}
	}()

	// Google wants audio, video and a data channel, in that order. One of each, nothing more.
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return nil, err
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		return nil, err
	}
	dc, err := pc.CreateDataChannel("dataSendChannel", nil)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	since := func() string { return time.Since(start).Round(100 * time.Millisecond).String() }
	dc.OnOpen(func() { c.logf("diag: datakanal åben (+%s)", since()) })
	dc.OnClose(func() { c.logf("diag: datakanal lukket (+%s)", since()) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		c.logf("diag: besked på datakanal (+%s): %q", since(), truncate(string(m.Data), 200))
	})
	pc.OnICEConnectionStateChange(func(st webrtc.ICEConnectionState) {
		msg := ""
		if st == webrtc.ICEConnectionStateConnected {
			if sctp := pc.SCTP(); sctp != nil {
				if pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
					msg = fmt.Sprintf(" via %s:%d (%s) -> %s:%d (%s)", pair.Local.Address, pair.Local.Port, pair.Local.Typ, pair.Remote.Address, pair.Remote.Port, pair.Remote.Typ)
				}
			}
		}
		c.logf("diag: ICE %s (+%s)%s", st, since(), msg)
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		dtls := ""
		if sctp := pc.SCTP(); sctp != nil {
			dtls = sctp.Transport().State().String()
		}
		c.logf("diag: forbindelse %s (+%s, DTLS %s)", st, since(), dtls)
		switch st {
		case webrtc.PeerConnectionStateConnected:
			s.connOnce.Do(func() { close(s.connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			s.kill(errICE)
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
		go func() { // drain RTCP so interceptors keep working
			for {
				if _, _, err := rcv.ReadRTCP(); err != nil {
					return
				}
			}
		}()
		isVideo := tr.Kind() == webrtc.RTPCodecTypeVideo
		if isVideo {
			s.videoSSR.Store(uint32(tr.SSRC()))
		}
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			if isVideo {
				s.lastRTP.Store(time.Now().UnixNano())
				s.firstOnce.Do(func() { close(s.firstRTP) })
				s.out.writeVideo(s, pkt)
			} else {
				s.out.writeAudio(s, pkt)
			}
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return nil, err
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	c.setState("starter")
	raw, err := c.g.Execute(ctx, c.slug, c.deviceID, cmdGenerate, map[string]string{"offerSdp": pc.LocalDescription().SDP}, PrioGenerate)
	if err != nil {
		return nil, err
	}
	c.countSession()
	var gr generateResult
	_ = json.Unmarshal(raw, &gr)
	s.MediaID = gr.MediaSessionID
	s.ExpiresAt, _ = time.Parse(time.RFC3339Nano, gr.ExpiresAt)
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = time.Now().Add(5 * time.Minute)
	}
	fail := func(e error) (*Session, error) {
		c.stopSession(s) // Google has a session open for us: close it politely if budget allows
		return nil, e
	}
	if gr.AnswerSdp == "" {
		return fail(errNoAnswer)
	}
	ans := gr.AnswerSdp
	c.logf("diag: svar-SDP: %s", sdpSummary(ans))
	if !strings.HasSuffix(ans, "\n") {
		ans += "\r\n"
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans}); err != nil {
		return fail(fmt.Errorf("Googles svar-SDP kunne ikke bruges: %w", err))
	}

	// Media watchdog: connected within 20 s, and video within 20 s after connected.
	select {
	case <-s.connected:
	case <-s.dead:
		return fail(errICE)
	case <-time.After(20 * time.Second):
		return fail(errICE)
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	select {
	case <-s.firstRTP:
	case <-s.dead:
		return fail(errICE)
	case <-time.After(20 * time.Second):
		return fail(errNoMedia)
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	ok = true
	go s.watchStall(c)
	return s, nil
}

func (s *Session) kill(err error) {
	s.deadOnce.Do(func() {
		s.deadErr = err
		close(s.dead)
	})
}

func (s *Session) close() {
	s.kill(errors.New("lukket"))
	_ = s.pc.Close()
}

// watchStall marks the session dead when video stops for 15 s, and logs receive stats every 15 s.
func (s *Session) watchStall(c *Camera) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var lastBytes uint64
	var lastFrames uint64
	lastLog := time.Now()
	for {
		select {
		case <-s.dead:
			return
		case <-t.C:
			if time.Since(time.Unix(0, s.lastRTP.Load())) > 15*time.Second {
				s.kill(errStalled)
				return
			}
			if time.Since(lastLog) < 15*time.Second {
				continue
			}
			for _, st := range s.pc.GetStats() {
				in, ok := st.(webrtc.InboundRTPStreamStats)
				if !ok || in.Kind != "video" {
					continue
				}
				secs := time.Since(lastLog).Seconds()
				frames := s.frames.Load()
				c.logf("diag: video %.0f kbit/s, %.1f fps ud, pakker %d (tabt %d), NACK %d, PLI %d, FIR %d",
					float64(in.BytesReceived-lastBytes)*8/1000/secs, float64(frames-lastFrames)/secs,
					in.PacketsReceived, in.PacketsLost, in.NACKCount, in.PLICount, in.FIRCount)
				lastBytes, lastFrames = in.BytesReceived, frames
			}
			lastLog = time.Now()
		}
	}
}

// requestKeyframe sends FIR + PLI (Google honours FIR), at most every 2 s.
func (s *Session) requestKeyframe() {
	now := time.Now().UnixNano()
	if now-s.lastFIR.Load() < int64(2*time.Second) {
		return
	}
	s.lastFIR.Store(now)
	ssrc := s.videoSSR.Load()
	if ssrc == 0 {
		return
	}
	seq := uint8(s.firSeq.Add(1))
	_ = s.pc.WriteRTCP([]rtcp.Packet{
		&rtcp.FullIntraRequest{MediaSSRC: ssrc, FIR: []rtcp.FIREntry{{SSRC: ssrc, SequenceNumber: seq}}},
		&rtcp.PictureLossIndication{MediaSSRC: ssrc},
	})
}

// extend asks Google for 5 more minutes. The response is always logged.
func (c *Camera) extendSession(ctx context.Context, s *Session) error {
	raw, err := c.g.Execute(ctx, c.slug, c.deviceID, cmdExtend, map[string]string{"mediaSessionId": s.MediaID}, PrioExtend)
	if err != nil {
		c.logf("forlængelse fejlede: %v", err)
		return err
	}
	var gr generateResult
	_ = json.Unmarshal(raw, &gr)
	if t, err := time.Parse(time.RFC3339Nano, gr.ExpiresAt); err == nil {
		s.ExpiresAt = t
	} else {
		s.ExpiresAt = time.Now().Add(5 * time.Minute)
	}
	if gr.MediaSessionID != "" {
		s.MediaID = gr.MediaSessionID
	}
	c.logf("forlænget til %s (svar: %s)", s.ExpiresAt.Local().Format("15:04:05"), truncate(string(raw), 200))
	return nil
}

// stopSession closes the session locally and tells Google, but only if there is budget right now.
func (c *Camera) stopSession(s *Session) {
	s.close()
	if s.MediaID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.g.Execute(ctx, c.slug, c.deviceID, cmdStop, map[string]string{"mediaSessionId": s.MediaID}, PrioStop); err != nil {
		if errors.Is(err, ErrNoBudget) {
			c.logf("stop sprunget over (ingen kvote); Google lukker selv sessionen ved udløb")
		}
	}
}

var _ = rtp.Packet{}

// sdpSummary keeps the lines that matter for debugging and drops ICE passwords and fingerprints.
func sdpSummary(sdp string) string {
	var keep []string
	for _, l := range strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "m="), strings.HasPrefix(l, "a=candidate"), strings.HasPrefix(l, "a=rtpmap"),
			strings.HasPrefix(l, "a=group"), strings.HasPrefix(l, "a=setup"), strings.HasPrefix(l, "a=ice-lite"),
			strings.HasPrefix(l, "a=extmap"), strings.HasPrefix(l, "a=rtcp-fb"), strings.HasPrefix(l, "a=fmtp"),
			l == "a=sendonly", l == "a=recvonly", l == "a=sendrecv", l == "a=inactive":
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, " | ")
}
