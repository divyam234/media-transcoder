package transcoder

import (
	"context"
	"io"
)

// Source describes media input. ReadSeeker and OpenReadSeeker are connected
// directly to libav through a custom AVIOContext, so seekable remote streams do
// not require materialization. OpenReadSeeker is preferred when libav may open
// the same source more than once because each open receives an independent
// stream.
type Source struct {
	Path           string
	ReadSeeker     io.ReadSeeker
	OpenReadSeeker func() (io.ReadSeekCloser, error)
	Name           string
}

func FromFile(path string) Source                        { return Source{Path: path, Name: path} }
func FromReadSeeker(name string, r io.ReadSeeker) Source { return Source{Name: name, ReadSeeker: r} }
func FromReadSeekerFactory(name string, open func() (io.ReadSeekCloser, error)) Source {
	return Source{Name: name, OpenReadSeeker: open}
}

// MediaInfo contains basic video-stream metadata.
type MediaInfo struct {
	Duration     float64 `json:"duration"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
	FPS          float64 `json:"fps"`
	AudioStreams int     `json:"audio_streams"`
	HasAudio     bool    `json:"has_audio"`
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}
