package transcoder

import (
	"context"
	"io"
)

// Source describes media input for the library. Path is fastest. ReadSeeker is
// materialized to a temp file so direct libavformat sees deterministic file input.
type Source struct {
	Path       string
	ReadSeeker io.ReadSeeker
	Name       string
}

func FromFile(path string) Source                        { return Source{Path: path, Name: path} }
func FromReadSeeker(name string, r io.ReadSeeker) Source { return Source{Name: name, ReadSeeker: r} }

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
