package transcoder

/*
#cgo pkg-config: libavformat libavcodec libavutil libavfilter
#cgo LDFLAGS: -lm
#include <stdlib.h>
#include "ffmpeg_bridge.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"unsafe"
)

func lastFFErr() string {
	s := C.tc_last_error()
	if s == nil {
		return "unknown ffmpeg error"
	}
	return C.GoString(s)
}

func Probe(path string) (MediaInfo, error) {
	cin := C.CString(path)
	defer C.free(unsafe.Pointer(cin))
	var info C.TCInfo
	if rc := C.tc_probe(cin, &info); rc < 0 {
		return MediaInfo{}, fmt.Errorf("probe failed: %s", lastFFErr())
	}
	return MediaInfo{Duration: float64(info.duration), Width: int(info.width), Height: int(info.height), FPS: float64(info.fps), AudioStreams: int(info.audio_streams), HasAudio: info.has_audio != 0}, nil
}

func cAudioMode(mode AudioMode) C.int {
	switch mode {
	case AudioCopy:
		return 1
	case AudioTranscode:
		return 2
	default:
		return 0
	}
}

func cBool(v bool) C.int {
	if v {
		return 1
	}
	return 0
}

func withCancelFlag(ctx context.Context, fn func(*C.int) C.int) C.int {
	flag := C.tc_cancel_alloc()
	if flag == nil {
		return C.int(-12)
	}
	done := make(chan struct{})
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				C.tc_cancel_set(flag)
			case <-done:
			}
		}()
	}
	rc := fn((*C.int)(unsafe.Pointer(flag)))
	close(done)
	C.tc_cancel_free(flag)
	return rc
}

func transcodeVideo(ctx context.Context, input, output string, opts TranscodeOptions) error {
	cin := C.CString(input)
	cout := C.CString(output)
	cpreset := C.CString(opts.Preset)
	cencoder := C.CString(opts.EncoderName)
	caudioCodec := C.CString(opts.AudioCodec)
	defer C.free(unsafe.Pointer(cin))
	defer C.free(unsafe.Pointer(cout))
	defer C.free(unsafe.Pointer(cpreset))
	defer C.free(unsafe.Pointer(cencoder))
	defer C.free(unsafe.Pointer(caudioCodec))
	copts := C.TCTranscodeOptions{
		target_width:   C.int(opts.Width),
		target_fps:     C.double(opts.FPS),
		crf:            C.int(opts.CRF),
		gop_size:       C.int(opts.GOPSize),
		max_b_frames:   C.int(opts.MaxBFrames),
		faststart:      cBool(opts.FastStart),
		preset:         cpreset,
		encoder_name:   cencoder,
		audio_mode:     cAudioMode(opts.AudioMode),
		audio_stream:   C.int(opts.AudioStream),
		audio_bitrate:  C.int(opts.AudioBitrate),
		audio_channels: C.int(opts.AudioChannels),
		audio_codec:    caudioCodec,
		start_time:     C.double(opts.StartTime),
		duration:       C.double(opts.Duration),
		cancel_flag:    nil,
	}
	rc := withCancelFlag(ctx, func(flag *C.int) C.int {
		copts.cancel_flag = (*C.int)(unsafe.Pointer(flag))
		return C.tc_transcode_video(cin, cout, &copts)
	})
	if rc < 0 {
		return fmt.Errorf("transcode video failed: %s", lastFFErr())
	}
	return nil
}

func transcodeHLSVideo(ctx context.Context, input, playlist, segmentPattern string, opts HLSOptions) error {
	cin := C.CString(input)
	cout := C.CString(playlist)
	cseg := C.CString(segmentPattern)
	cpreset := C.CString(opts.Preset)
	cencoder := C.CString(opts.EncoderName)
	cplaylistType := C.CString(opts.PlaylistType)
	csegmentType := C.CString(opts.SegmentType)
	caudioCodec := C.CString(opts.AudioCodec)
	defer C.free(unsafe.Pointer(cin))
	defer C.free(unsafe.Pointer(cout))
	defer C.free(unsafe.Pointer(cseg))
	defer C.free(unsafe.Pointer(cpreset))
	defer C.free(unsafe.Pointer(cencoder))
	defer C.free(unsafe.Pointer(cplaylistType))
	defer C.free(unsafe.Pointer(csegmentType))
	defer C.free(unsafe.Pointer(caudioCodec))
	copts := C.TCTranscodeOptions{
		target_width:      C.int(opts.Width),
		target_fps:        C.double(opts.FPS),
		crf:               C.int(opts.CRF),
		gop_size:          C.int(opts.GOPSize),
		max_b_frames:      C.int(opts.MaxBFrames),
		faststart:         0,
		preset:            cpreset,
		encoder_name:      cencoder,
		hls_time:          C.double(opts.SegmentSeconds),
		hls_list_size:     C.int(opts.ListSize),
		hls_playlist_type: cplaylistType,
		hls_segment_type:  csegmentType,
		audio_mode:        cAudioMode(opts.AudioMode),
		audio_stream:      C.int(opts.AudioStream),
		audio_bitrate:     C.int(opts.AudioBitrate),
		audio_channels:    C.int(opts.AudioChannels),
		audio_codec:       caudioCodec,
		start_time:        C.double(opts.StartTime),
		duration:          C.double(opts.Duration),
		cancel_flag:       nil,
	}
	rc := withCancelFlag(ctx, func(flag *C.int) C.int {
		copts.cancel_flag = (*C.int)(unsafe.Pointer(flag))
		return C.tc_transcode_hls_video(cin, cout, cseg, &copts)
	})
	if rc < 0 {
		return fmt.Errorf("transcode HLS failed: %s", lastFFErr())
	}
	return nil
}

func EncoderAvailable(name string) bool {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	return C.tc_encoder_available(cname) != 0
}

func transcodeDASHVideo(ctx context.Context, input, mpd string, opts DASHOptions) error {
	cin := C.CString(input)
	cout := C.CString(mpd)
	cpreset := C.CString(opts.Preset)
	cencoder := C.CString(opts.EncoderName)
	caudioCodec := C.CString(opts.AudioCodec)
	defer C.free(unsafe.Pointer(cin))
	defer C.free(unsafe.Pointer(cout))
	defer C.free(unsafe.Pointer(cpreset))
	defer C.free(unsafe.Pointer(cencoder))
	defer C.free(unsafe.Pointer(caudioCodec))
	copts := C.TCTranscodeOptions{
		target_width:   C.int(opts.Width),
		target_fps:     C.double(opts.FPS),
		crf:            C.int(opts.CRF),
		gop_size:       C.int(opts.GOPSize),
		max_b_frames:   C.int(opts.MaxBFrames),
		preset:         cpreset,
		encoder_name:   cencoder,
		audio_mode:     cAudioMode(opts.AudioMode),
		audio_stream:   C.int(opts.AudioStream),
		audio_bitrate:  C.int(opts.AudioBitrate),
		audio_channels: C.int(opts.AudioChannels),
		audio_codec:    caudioCodec,
	}
	rc := withCancelFlag(ctx, func(flag *C.int) C.int {
		copts.cancel_flag = (*C.int)(unsafe.Pointer(flag))
		return C.tc_transcode_dash_video(cin, cout, &copts)
	})
	if rc < 0 {
		return fmt.Errorf("transcode DASH failed: %s", lastFFErr())
	}
	return nil
}

func transcodeSegmentVideo(ctx context.Context, input, output string, opts TranscodeOptions) error {
	cin := C.CString(input)
	cout := C.CString(output)
	cpreset := C.CString(opts.Preset)
	cencoder := C.CString(opts.EncoderName)
	caudioCodec := C.CString(opts.AudioCodec)
	defer C.free(unsafe.Pointer(cin))
	defer C.free(unsafe.Pointer(cout))
	defer C.free(unsafe.Pointer(cpreset))
	defer C.free(unsafe.Pointer(cencoder))
	defer C.free(unsafe.Pointer(caudioCodec))
	copts := C.TCTranscodeOptions{
		target_width:   C.int(opts.Width),
		target_fps:     C.double(opts.FPS),
		crf:            C.int(opts.CRF),
		gop_size:       C.int(opts.GOPSize),
		max_b_frames:   C.int(opts.MaxBFrames),
		faststart:      0,
		preset:         cpreset,
		encoder_name:   cencoder,
		audio_mode:     cAudioMode(opts.AudioMode),
		audio_stream:   C.int(opts.AudioStream),
		audio_bitrate:  C.int(opts.AudioBitrate),
		audio_channels: C.int(opts.AudioChannels),
		audio_codec:    caudioCodec,
		start_time:     C.double(opts.StartTime),
		duration:       C.double(opts.Duration),
		cancel_flag:    nil,
	}
	rc := withCancelFlag(ctx, func(flag *C.int) C.int {
		copts.cancel_flag = (*C.int)(unsafe.Pointer(flag))
		return C.tc_transcode_segment_video(cin, cout, &copts)
	})
	if rc < 0 {
		return fmt.Errorf("transcode segment failed: %s", lastFFErr())
	}
	return nil
}
func transcodeFMP4SegmentVideo(ctx context.Context, input, output string, opts TranscodeOptions) error {
	cin := C.CString(input)
	cout := C.CString(output)
	cpreset := C.CString(opts.Preset)
	cencoder := C.CString(opts.EncoderName)
	caudioCodec := C.CString(opts.AudioCodec)
	defer C.free(unsafe.Pointer(cin))
	defer C.free(unsafe.Pointer(cout))
	defer C.free(unsafe.Pointer(cpreset))
	defer C.free(unsafe.Pointer(cencoder))
	defer C.free(unsafe.Pointer(caudioCodec))
	copts := C.TCTranscodeOptions{
		target_width:   C.int(opts.Width),
		target_fps:     C.double(opts.FPS),
		crf:            C.int(opts.CRF),
		gop_size:       C.int(opts.GOPSize),
		max_b_frames:   C.int(opts.MaxBFrames),
		faststart:      0,
		preset:         cpreset,
		encoder_name:   cencoder,
		audio_mode:     cAudioMode(opts.AudioMode),
		audio_stream:   C.int(opts.AudioStream),
		audio_bitrate:  C.int(opts.AudioBitrate),
		audio_channels: C.int(opts.AudioChannels),
		audio_codec:    caudioCodec,
		start_time:     C.double(opts.StartTime),
		duration:       C.double(opts.Duration),
		cancel_flag:    nil,
	}
	rc := withCancelFlag(ctx, func(flag *C.int) C.int {
		copts.cancel_flag = (*C.int)(unsafe.Pointer(flag))
		return C.tc_transcode_fmp4_segment_video(cin, cout, &copts)
	})
	if rc < 0 {
		return fmt.Errorf("transcode fMP4 segment failed: %s", lastFFErr())
	}
	return nil
}

func runtimeFFmpegCapabilities() (RuntimeFFmpegCapabilities, error) {
	cj := C.tc_runtime_capabilities_json()
	if cj == nil {
		return RuntimeFFmpegCapabilities{}, fmt.Errorf("runtime capabilities failed: %s", lastFFErr())
	}
	defer C.tc_free(unsafe.Pointer(cj))
	var caps RuntimeFFmpegCapabilities
	if err := json.Unmarshal([]byte(C.GoString(cj)), &caps); err != nil {
		return RuntimeFFmpegCapabilities{}, err
	}
	return caps, nil
}
