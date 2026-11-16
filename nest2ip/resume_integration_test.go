//go:build integration

package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// Video, then a 4 s hole (frozen, stamped picture), then video again in the same session,
// resuming mid-GOP. A viewer across the whole sequence must decode without errors.
func TestResumeAfterFreeze(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NEST2IP_DATA", dir)
	es := dir + "/in.h264"
	if o, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=15",
		"-t", "20", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-profile:v", "baseline", "-g", "30",
		"-bsf:v", "h264_metadata=aud=insert", "-f", "h264", es).CombinedOutput(); err != nil {
		t.Fatalf("gen: %v %s", err, o)
	}
	raw, _ := os.ReadFile(es)
	var aus [][][]byte
	var cur [][]byte
	for _, n := range splitAnnexB(raw) {
		if h264.NALUType(n[0]&0x1f) == h264.NALUTypeAccessUnitDelimiter {
			if len(cur) > 0 {
				aus = append(aus, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, n)
	}
	aus = append(aus, cur)

	cam := newCamera(CameraConfig{Name: "Resume", Port: 18603, Path: "r"}, nil, nil, time.Minute)
	out, err := newOutput(cam, 18603, 58104)
	if err != nil {
		t.Fatal(err)
	}
	cam.out = out
	defer out.close()
	s := &Session{ID: 1, dead: make(chan struct{})}
	out.activate(s)
	f := &format.H264{PayloadTyp: 102, PacketizationMode: 1}
	enc, _ := f.CreateEncoder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ts := uint32(1000)
		for i, au := range aus {
			if ctx.Err() != nil {
				return
			}
			if i == 75 { // after 5 s: a 4 s hole, then resume mid-GOP (frame 75 is not a keyframe)
				time.Sleep(4 * time.Second)
				ts += 4 * 90000
			}
			pkts, _ := enc.Encode(au)
			for _, p := range pkts {
				p.Timestamp = ts
				out.writeVideo(s, p)
			}
			ts += 90000 / 15
			time.Sleep(time.Second / 15)
		}
	}()
	<-out.ready
	time.Sleep(4 * time.Second) // connect while video flows, then sit through the hole
	o, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-rtsp_transport", "tcp",
		"-i", "rtsp://127.0.0.1:18603/r", "-t", "9", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, o)
	}
	for _, bad := range []string{"decode_slice_header", "out of range", "illegal", "no frame"} {
		if strings.Contains(string(o), bad) {
			t.Fatalf("decoding errors across the freeze:\n%s", o)
		}
	}
}
