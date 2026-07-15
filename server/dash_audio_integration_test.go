package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	transcoder "media-transcoder"
)

func TestOptionalLibraryDASHWithSeparateAudio(t *testing.T) {
	input := os.Getenv("TRANSCODER_TEST_VAAPI_INPUT")
	if input == "" {
		input = "/home/bhunter/Downloads/Lucky 2026 S01E01 1080p 10bit WEBRip 6CH x265 HEVC-PSA.mkv"
	}
	device := os.Getenv("TRANSCODER_TEST_VAAPI_DEVICE")
	if device == "" {
		device = "/dev/dri/renderD128"
	}
	if _, err := os.Stat(input); err != nil {
		t.Skipf("input unavailable: %v", err)
	}
	if _, err := os.Stat(device); err != nil {
		t.Skipf("VAAPI unavailable: %v", err)
	}
	root := filepath.Dir(input)
	name := filepath.Base(input)
	s := New(Config{
		RequestTimeout:    2 * time.Minute,
		MaxConcurrentJobs: 2,
		CacheRoot:         t.TempDir(),
		AllowedInputRoots: []string{root},
		Libraries:         map[string]LibraryConfig{"media": {VFS: root}},
		Profiles: map[string]PlaybackProfile{"dash": {
			Container:      "dash",
			SegmentSeconds: 4,
			Audio:          AudioProfile{Mode: transcoder.AudioTranscode, Codec: "aac", Bitrate: 128000, Channels: 2},
			Video:          VideoProfile{EncoderName: "h264_vaapi", HardwareDevice: device, HardwareDecode: true, CRF: 24, GOPSize: 96},
			Variants:       []transcoder.LadderVariant{{Name: "720p", Width: 1280, VideoBitrate: 2800000}},
		}},
	})
	t.Cleanup(s.Close)
	base := "/play/dash/dash/media/" + url.PathEscape(name)
	mpd := request(t, s, base+"/manifest.mpd")
	if mpd.Code != http.StatusOK {
		t.Fatalf("manifest status=%d body=%s", mpd.Code, mpd.Body.String())
	}
	body := mpd.Body.String()
	sess := onlyDASHSession(t, s)
	if len(sess.Variants) != 1 || sess.Variants[0].Codec.CodecString == "" {
		t.Fatalf("video codec was not probed from init segment: %+v", sess.Variants)
	}
	if sess.AudioCodec.CodecString == "" {
		t.Fatalf("audio codec was not probed from init segment: %+v", sess.AudioCodec)
	}
	if !strings.Contains(body, `codecs="`+sess.Variants[0].Codec.CodecString+`"`) {
		t.Fatalf("manifest missing probed video codec %q:\n%s", sess.Variants[0].Codec.CodecString, body)
	}
	if !strings.Contains(body, `codecs="`+sess.AudioCodec.CodecString+`"`) {
		t.Fatalf("manifest missing probed audio codec %q:\n%s", sess.AudioCodec.CodecString, body)
	}
	if sess.AudioCodec.SampleRate != 48000 || sess.AudioCodec.Channels != 2 {
		t.Fatalf("unexpected probed audio descriptor: %+v", sess.AudioCodec)
	}
	if !strings.Contains(body, `mimeType="audio/mp4"`) || !strings.Contains(body, `audio/segment/init.mp4`) {
		t.Fatalf("manifest missing separate audio adaptation set:\n%s", body)
	}
	for _, path := range []string{"/audio/segment/init.mp4", "/audio/segment/000000.m4s", "/variant/720p/segment/init.mp4", "/variant/720p/segment/000000.m4s"} {
		rr := request(t, s, base+path)
		if rr.Code != http.StatusOK || rr.Body.Len() == 0 {
			t.Fatalf("%s status=%d bytes=%d body=%s", path, rr.Code, rr.Body.Len(), rr.Body.String())
		}
	}
}

func onlyDASHSession(t *testing.T, s *Server) *DynamicDASHSession {
	t.Helper()
	s.dynDASH.mu.RLock()
	defer s.dynDASH.mu.RUnlock()
	if len(s.dynDASH.sessions) != 1 {
		t.Fatalf("expected one DASH session, got %d", len(s.dynDASH.sessions))
	}
	for _, sess := range s.dynDASH.sessions {
		return sess
	}
	return nil
}
