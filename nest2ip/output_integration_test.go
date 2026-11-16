//go:build integration

package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// Feeds a synthetic H264 stream (as if it came from Google) into an Output and checks that
// ffmpeg can read the RTSP main and muted paths, and that snapshots work.
// Needs ffmpeg in PATH. Run: go test -tags integration -run TestOutput -v
func TestOutputEndToEnd(t *testing.T) {
	dir := t.TempDir()
	es := dir + "/in.h264"
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=15",
		"-t", "20", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-profile:v", "baseline", "-g", "30", "-bsf:v", "h264_metadata=aud=insert", "-f", "h264", es)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg gen: %v %s", err, out)
	}
	raw, _ := os.ReadFile(es)
	nalus := splitAnnexB(raw)
	// split into access units on AUD
	var aus [][][]byte
	var cur [][]byte
	for _, n := range nalus {
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

	cam := newCamera(CameraConfig{Name: "Test", Port: 18554, Path: "secret", MutedPath: "mutedsecret"}, nil, nil, time.Minute)
	out, err := newOutput(cam, 18554, 58000)
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
		ts := uint32(12345)
		for _, au := range aus {
			if ctx.Err() != nil {
				return
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

	select {
	case <-out.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never became ready")
	}
	// Media3 (TVBliz) rejects an empty session name ("s= "), so DESCRIBE must carry a real one.
	for _, p := range []string{"secret", "mutedsecret"} {
		conn, err := net.Dial("tcp", "127.0.0.1:18554")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte("DESCRIBE rtsp://127.0.0.1:18554/" + p + " RTSP/1.0\r\nCSeq: 1\r\nAccept: application/sdp\r\n\r\n"))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 8192)
		n, _ := conn.Read(buf)
		conn.Close()
		if !bytes.Contains(buf[:n], []byte("\r\ns=nest2ip\r\n")) {
			t.Fatalf("%s: SDP has no s=nest2ip line:\n%s", p, buf[:n])
		}
	}
	for _, p := range []string{"secret", "mutedsecret"} {
		cmd := exec.Command("ffprobe", "-hide_banner", "-loglevel", "error", "-rtsp_transport", "udp",
			"-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", "rtsp://127.0.0.1:18554/"+p)
		o, err := cmd.CombinedOutput()
		if err != nil || !bytes.Contains(o, []byte("h264,640,360")) {
			t.Fatalf("ffprobe %s: %v %s", p, err, o)
		}
		t.Logf("%s: %s", p, bytes.TrimSpace(o))
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-rtsp_transport", "tcp",
		"-i", "rtsp://127.0.0.1:18554/secret", "-frames:v", "20", "-f", "null", "-")
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg read 20 frames over TCP: %v %s", err, o)
	}
	if o, err := exec.Command("ffprobe", "-loglevel", "error", "rtsp://127.0.0.1:18554/wrong").CombinedOutput(); err == nil {
		t.Fatalf("unknown path must fail: %s", o)
	}
	jpg, _, err := out.Snapshot(context.Background())
	if err != nil || len(jpg) < 1000 || jpg[0] != 0xFF || jpg[1] != 0xD8 {
		t.Fatalf("snapshot: %v len=%d", err, len(jpg))
	}
	t.Logf("snapshot %d bytes", len(jpg))

	// Google's video stops: viewers must keep getting (repeated) keyframes instead of timing out.
	cancel()
	time.Sleep(3 * time.Second)
	cmd = exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-rtsp_transport", "tcp", "-timeout", "5000000",
		"-i", "rtsp://127.0.0.1:18554/secret", "-frames:v", "4", "-f", "null", "-")
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("no frames while Google is gone: %v %s", err, o)
	}

	// The frozen picture gets "FROSSET" and its time burned in, and viewers receive that version.
	deadline := time.Now().Add(15 * time.Second)
	for {
		out.mu.Lock()
		ok := out.stampedKey != nil
		out.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stamped frozen picture was never made")
		}
		time.Sleep(200 * time.Millisecond)
	}
	shot := dir + "/frozen.jpg"
	cmd = exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-rtsp_transport", "tcp", "-timeout", "5000000",
		"-i", "rtsp://127.0.0.1:18554/secret", "-frames:v", "1", shot)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("read stamped frozen frame: %v %s", err, o)
	}
	if dst := os.Getenv("NEST2IP_SAVE_FROZEN"); dst != "" {
		b, _ := os.ReadFile(shot)
		_ = os.WriteFile(dst, b, 0o644)
	}
	jpg, _, err = out.Snapshot(context.Background())
	if err != nil || len(jpg) < 1000 {
		t.Fatalf("stamped snapshot: %v", err)
	}
}
