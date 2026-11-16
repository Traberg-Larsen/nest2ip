//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

// A keyframe saved by the previous run is served from the first second, before Google answers,
// and an empty data dir gives a placeholder, so RTSP clients never sit without frames.
func TestSavedFrameAndPlaceholder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NEST2IP_DATA", dir)
	kf := dir + "/kf.h264"
	if o, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=1",
		"-frames:v", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-f", "h264", kf).CombinedOutput(); err != nil {
		t.Fatalf("gen: %v %s", err, o)
	}
	b, _ := os.ReadFile(kf)
	_ = os.WriteFile(dir+"/frame-saved.h264", b, 0o600)
	meta, _ := json.Marshal(savedFrameMeta{At: time.Now().Add(-time.Hour)})
	_ = os.WriteFile(dir+"/frame-saved.json", meta, 0o600)

	probe := func(port, path string) string {
		o, err := exec.Command("ffprobe", "-hide_banner", "-loglevel", "error", "-rtsp_transport", "tcp",
			"-show_entries", "stream=codec_name,width,height", "-of", "csv=p=0", "rtsp://127.0.0.1:"+port+"/"+path).CombinedOutput()
		if err != nil {
			t.Fatalf("ffprobe %s: %v %s", port, err, o)
		}
		return string(bytes.TrimSpace(o))
	}

	cam := newCamera(CameraConfig{Name: "Saved", Port: 18601, Path: "p"}, nil, nil, time.Minute)
	out, err := newOutput(cam, 18601, 58100)
	if err != nil {
		t.Fatal(err)
	}
	cam.out = out
	defer out.close()
	select {
	case <-out.ready:
	default:
		t.Fatal("saved frame should make the stream ready at once")
	}
	if got := probe("18601", "p"); got != "h264,640,360" {
		t.Fatalf("saved frame: got %q", got)
	}

	cam2 := newCamera(CameraConfig{Name: "Fresh", Port: 18602, Path: "q"}, nil, nil, time.Minute)
	out2, err := newOutput(cam2, 18602, 58102)
	if err != nil {
		t.Fatal(err)
	}
	cam2.out = out2
	defer out2.close()
	select {
	case <-out2.ready:
	case <-time.After(15 * time.Second):
		t.Fatal("placeholder never made the stream ready")
	}
	if got := probe("18602", "q"); got != "h264,1920,1080" {
		t.Fatalf("placeholder: got %q", got)
	}
}
