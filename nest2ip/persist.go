package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// The last keyframe of each camera is kept on disk so RTSP viewers get a picture from the
// moment nest2ip starts, before Google has answered (or while it is down). It is written at
// most every 5 minutes, and on shutdown, to spare the SD card.

type savedFrameMeta struct {
	At    time.Time `json:"at"`
	Audio bool      `json:"audio"`
}

func dataDir() string { return env("NEST2IP_DATA", "/data") }

func (o *Output) framePaths() (string, string) {
	base := filepath.Join(dataDir(), "frame-"+o.cam.slug)
	return base + ".h264", base + ".json"
}

// saveFrameLocked stores the current keyframe. Without force it does so at most every 5 minutes.
func (o *Output) saveFrameLocked(force bool) {
	if o.lastKeyAU == nil || o.placeholder {
		return
	}
	if !force && time.Since(o.lastSaved) < 5*time.Minute {
		return
	}
	o.lastSaved = time.Now()
	au := append([]byte(nil), o.lastKeyAU...)
	meta, _ := json.Marshal(savedFrameMeta{At: o.lastKeyAt, Audio: o.audioSeen})
	write := func() {
		h, m := o.framePaths()
		if err := os.WriteFile(h+".tmp", au, 0o600); err == nil {
			_ = os.Rename(h+".tmp", h)
			_ = os.WriteFile(m, meta, 0o600)
		}
	}
	if force {
		write()
	} else {
		go write()
	}
}

// keyframeParts splits an Annex-B keyframe into SPS, PPS and the NAL units to send.
func keyframeParts(au []byte) (sps, pps []byte, nalus [][]byte, ok bool) {
	idr := false
	for _, n := range splitAnnexB(au) {
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
		default:
			continue
		}
		nalus = append(nalus, n)
	}
	return sps, pps, nalus, sps != nil && pps != nil && idr
}

// useStillLocked makes au (a keyframe that is not live video) the picture viewers get until
// Google's video arrives, and opens the RTSP streams with it.
func (o *Output) useStillLocked(au []byte, at time.Time, placeholder bool) bool {
	sps, pps, nalus, ok := keyframeParts(au)
	if !ok || o.main != nil || (placeholder && o.sps != nil) { // live video already on its way
		return false
	}
	o.sps, o.pps = sps, pps
	o.lastKeyAU, o.lastKey, o.lastKeyAt = au, nalus, at
	o.placeholder = placeholder
	if err := o.createStreamsLocked(); err != nil {
		o.cam.logf("RTSP-strøm kunne ikke oprettes: %v", err)
		return false
	}
	return true
}

// loadSavedFrame serves the keyframe from the last run, if there is one.
func (o *Output) loadSavedFrame() bool {
	h, m := o.framePaths()
	au, err := os.ReadFile(h)
	if err != nil {
		return false
	}
	var meta savedFrameMeta
	if b, err := os.ReadFile(m); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.audioSeen = meta.Audio
	if !o.useStillLocked(au, meta.At, false) {
		return false
	}
	o.cam.logf("viser gemt billede fra %s, indtil Google sender video", meta.At.Local().Format("02.01.2006 15:04:05"))
	return true
}

// makePlaceholder is used on the very first start, when no picture has been saved yet.
func (o *Output) makePlaceholder() {
	select { // give Google a moment first; most starts get live video quickly
	case <-o.done:
		return
	case <-time.After(3 * time.Second):
	}
	o.mu.Lock()
	live := o.sps != nil
	o.mu.Unlock()
	if live {
		return
	}
	vf := "drawtext=fontfile=" + stampFont + ":text='Venter på video fra Google':fontcolor=white:fontsize=56:x=(w-text_w)/2:y=(h-text_h)/2"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=0x202020:s=1920x1080:r=1", "-frames:v", "1", "-vf", vf,
		"-c:v", "libx264", "-preset", "ultrafast", "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-x264-params", "keyint=1", "-f", "h264", "pipe:1")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		o.cam.logf("kunne ikke lave ventebillede: %v", err)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.useStillLocked(out.Bytes(), time.Time{}, true) {
		o.cam.logf("viser ventebillede, indtil Google sender video")
	}
}
