package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"
)

// Output is the RTSP side of one camera: one server on the camera's own port with a
// main path (video + audio) and an optional muted path (video only). All RTSP clients
// share the single Google session; clients never cause Google calls.
type Output struct {
	cam       *Camera
	path      string
	mutedPath string
	server    *gortsplib.Server

	mu         sync.Mutex
	ready      chan struct{} // closed when the streams exist (first SPS/PPS seen)
	readyOnce  sync.Once
	main       *gortsplib.ServerStream
	muted      *gortsplib.ServerStream
	vMain      *description.Media
	aMain      *description.Media
	vMuted     *description.Media
	fMain      *format.H264
	fMuted     *format.H264
	enc        *rtph264.Encoder
	audioSeq   uint16
	audioSSRC  uint32
	start      time.Time
	active     *Session
	dec        *rtph264.Decoder
	waitingKey bool
	vMap, aMap tsMap
	sps, pps   []byte
	audioSeen  bool      // the camera sends audio: include an audio track in the RTSP description
	firstVideo time.Time // when we first had SPS/PPS; we wait up to 3 s for audio before describing

	lastFrame  time.Time
	lastKeyAU  []byte // Annex-B, SPS+PPS+IDR
	lastKeyAt  time.Time
	snapJPEG   []byte
	snapFromAt time.Time

	playing map[*gortsplib.ServerSession]bool
}

// tsMap maps one session's RTP timestamps onto our continuous output timeline.
type tsMap struct {
	set     bool
	firstIn uint32
	base    uint32
}

func (m *tsMap) out(in uint32, clock float64, start time.Time) uint32 {
	if !m.set {
		m.set = true
		m.firstIn = in
		m.base = uint32(time.Since(start).Seconds() * clock)
	}
	return m.base + (in - m.firstIn)
}

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

func newOutput(cam *Camera, port, udpBase int) (*Output, error) {
	o := &Output{
		cam:       cam,
		path:      cam.cfg.Path,
		mutedPath: cam.cfg.MutedPath,
		ready:     make(chan struct{}),
		audioSSRC: randUint32(),
		start:     time.Now(),
		playing:   map[*gortsplib.ServerSession]bool{},
	}
	o.server = &gortsplib.Server{
		Handler:        o,
		RTSPAddress:    fmt.Sprintf(":%d", port),
		UDPRTPAddress:  fmt.Sprintf(":%d", udpBase),
		UDPRTCPAddress: fmt.Sprintf(":%d", udpBase+1),
		WriteQueueSize: 2048,
	}
	if err := o.server.Start(); err != nil {
		return nil, fmt.Errorf("RTSP-port %d: %w", port, err)
	}
	return o, nil
}

func (o *Output) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.main != nil {
		o.main.Close()
	}
	if o.muted != nil {
		o.muted.Close()
	}
	o.server.Close()
}

// createStreamsLocked builds the RTSP descriptions once SPS/PPS are known.
func (o *Output) createStreamsLocked() error {
	o.fMain = &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: o.sps, PPS: o.pps}
	o.vMain = &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{o.fMain}}
	medias := []*description.Media{o.vMain}
	if o.audioSeen {
		o.aMain = &description.Media{Type: description.MediaTypeAudio, Formats: []format.Format{&format.Opus{PayloadTyp: 97, ChannelCount: 2}}}
		medias = append(medias, o.aMain)
	}
	o.main = &gortsplib.ServerStream{Server: o.server, Desc: &description.Session{Medias: medias}}
	if err := o.main.Initialize(); err != nil {
		return err
	}
	if o.mutedPath != "" {
		o.fMuted = &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: o.sps, PPS: o.pps}
		o.vMuted = &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{o.fMuted}}
		o.muted = &gortsplib.ServerStream{Server: o.server, Desc: &description.Session{Medias: []*description.Media{o.vMuted}}}
		if err := o.muted.Initialize(); err != nil {
			return err
		}
	}
	enc, err := o.fMain.CreateEncoder()
	if err != nil {
		return err
	}
	o.enc = enc
	o.readyOnce.Do(func() { close(o.ready) })
	return nil
}

// activate makes s the session whose packets are sent out. The previous one is ignored from now on.
func (o *Output) activate(s *Session) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active = s
	o.dec = nil
	o.waitingKey = true
	o.vMap = tsMap{}
	o.aMap = tsMap{}
}

func (o *Output) writeVideo(s *Session, pkt *rtp.Packet) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if s != o.active {
		return
	}
	if o.dec == nil {
		f := &format.H264{PayloadTyp: pkt.PayloadType, PacketizationMode: 1}
		d, err := f.CreateDecoder()
		if err != nil {
			return
		}
		o.dec = d
	}
	au, err := o.dec.Decode(pkt)
	if err != nil || len(au) == 0 {
		return // more packets needed, or a damaged fragment
	}
	var sps, pps []byte
	idr := false
	for _, n := range au {
		if len(n) == 0 {
			continue
		}
		switch h264.NALUType(n[0] & 0x1f) {
		case h264.NALUTypeSPS:
			sps = n
		case h264.NALUTypePPS:
			pps = n
		case h264.NALUTypeIDR:
			idr = true
		}
	}
	if sps != nil && !bytes.Equal(sps, o.sps) {
		o.sps = append([]byte(nil), sps...)
		o.paramsChangedLocked()
	}
	if pps != nil && !bytes.Equal(pps, o.pps) {
		o.pps = append([]byte(nil), pps...)
		o.paramsChangedLocked()
	}
	if o.main == nil {
		if o.sps == nil || o.pps == nil {
			go s.requestKeyframe()
			return
		}
		if o.firstVideo.IsZero() {
			o.firstVideo = time.Now()
		}
		if !o.audioSeen && time.Since(o.firstVideo) < 3*time.Second {
			return // give audio a moment to show up, so clients don't wait for a track that never comes
		}
		if err := o.createStreamsLocked(); err != nil {
			o.cam.logf("RTSP-strøm kunne ikke oprettes: %v", err)
			return
		}
		o.cam.logf("RTSP-strøm klar (lyd: %v)", o.audioSeen)
	}
	if o.waitingKey {
		if !idr && !h264.IsRandomAccess(au) {
			go s.requestKeyframe()
			return
		}
		o.waitingKey = false
	}
	if idr && sps == nil {
		au = append([][]byte{o.sps, o.pps}, au...) // late joiners always get parameters
	}
	ts := o.vMap.out(pkt.Timestamp, 90000, o.start)
	pkts, err := o.enc.Encode(au)
	if err != nil {
		return
	}
	for _, p := range pkts {
		p.Timestamp = ts
		_ = o.main.WritePacketRTP(o.vMain, p)
		if o.muted != nil {
			_ = o.muted.WritePacketRTP(o.vMuted, p)
		}
	}
	now := time.Now()
	o.lastFrame = now
	s.frames.Add(1)
	if idr {
		var buf bytes.Buffer
		for _, n := range au {
			buf.Write([]byte{0, 0, 0, 1})
			buf.Write(n)
		}
		o.lastKeyAU = buf.Bytes()
		o.lastKeyAt = now
	}
}

func (o *Output) paramsChangedLocked() {
	if o.main == nil || o.sps == nil || o.pps == nil {
		return
	}
	o.fMain.SafeSetParams(o.sps, o.pps)
	o.main.ReloadDesc()
	if o.fMuted != nil {
		o.fMuted.SafeSetParams(o.sps, o.pps)
		o.muted.ReloadDesc()
	}
}

func (o *Output) writeAudio(s *Session, pkt *rtp.Packet) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if s != o.active {
		return
	}
	o.audioSeen = true
	if o.main == nil || o.aMain == nil || o.waitingKey {
		return
	}
	o.audioSeq++
	p := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         pkt.Marker,
			PayloadType:    97,
			SequenceNumber: o.audioSeq,
			Timestamp:      o.aMap.out(pkt.Timestamp, 48000, o.start),
			SSRC:           o.audioSSRC,
		},
		Payload: pkt.Payload,
	}
	_ = o.main.WritePacketRTP(o.aMain, p)
}

func (o *Output) status() (lastFrame time.Time, clients int, ready bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lastFrame, len(o.playing), o.main != nil
}

// Snapshot decodes the last keyframe with ffmpeg. It never calls Google.
func (o *Output) Snapshot(ctx context.Context) ([]byte, time.Time, error) {
	o.mu.Lock()
	au, at := o.lastKeyAU, o.lastKeyAt
	if o.snapJPEG != nil && o.snapFromAt.Equal(at) {
		j := o.snapJPEG
		o.mu.Unlock()
		return j, at, nil
	}
	active := o.active
	o.mu.Unlock()
	if active != nil {
		go active.requestKeyframe() // so the next snapshot is fresher
	}
	if au == nil {
		return nil, time.Time{}, errors.New("intet billede endnu")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "h264", "-i", "pipe:0", "-frames:v", "1", "-q:v", "4", "-f", "image2", "-c:v", "mjpeg", "pipe:1")
	cmd.Stdin = bytes.NewReader(au)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, time.Time{}, fmt.Errorf("ffmpeg: %v %s", err, truncate(stderr.String(), 200))
	}
	o.mu.Lock()
	o.snapJPEG, o.snapFromAt = out.Bytes(), at
	o.mu.Unlock()
	return out.Bytes(), at, nil
}

// --- RTSP server handler ---

func (o *Output) streamFor(path string) *gortsplib.ServerStream {
	p := strings.Trim(path, "/")
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case p == o.path || strings.HasPrefix(p, o.path+"/"):
		return o.main
	case o.mutedPath != "" && (p == o.mutedPath || strings.HasPrefix(p, o.mutedPath+"/")):
		return o.muted
	}
	return nil
}

func (o *Output) knownPath(path string) bool {
	p := strings.Trim(path, "/")
	return p == o.path || strings.HasPrefix(p, o.path+"/") ||
		(o.mutedPath != "" && (p == o.mutedPath || strings.HasPrefix(p, o.mutedPath+"/")))
}

func (o *Output) waitStream(path string) (*base.Response, *gortsplib.ServerStream, error) {
	if !o.knownPath(path) {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	o.cam.demand() // on-demand cameras start here; always-on cameras are already running
	select {
	case <-o.ready:
	case <-time.After(25 * time.Second):
		return &base.Response{StatusCode: base.StatusServiceUnavailable}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, o.streamFor(path), nil
}

func (o *Output) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return o.waitStream(ctx.Path)
}

func (o *Output) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return o.waitStream(ctx.Path)
}

func (o *Output) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	o.mu.Lock()
	o.playing[ctx.Session] = true
	active := o.active
	o.mu.Unlock()
	o.cam.clientsChanged()
	if active != nil {
		go active.requestKeyframe() // new viewers get a picture fast
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (o *Output) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	o.mu.Lock()
	_, was := o.playing[ctx.Session]
	delete(o.playing, ctx.Session)
	o.mu.Unlock()
	if was {
		o.cam.clientsChanged()
	}
}
