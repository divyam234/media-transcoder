package transcoder

import (
	"context"
	"errors"
	"io"
)

type preparedSource struct {
	path    string
	cleanup func()
}

func prepareSource(ctx context.Context, src Source) (preparedSource, error) {
	if src.Path != "" {
		return preparedSource{path: src.Path}, nil
	}
	if src.ReadSeeker == nil {
		return preparedSource{}, errors.New("source must have Path or ReadSeeker")
	}
	if err := checkContext(ctx); err != nil {
		return preparedSource{}, err
	}
	_, _ = src.ReadSeeker.Seek(0, io.SeekStart)
	path, cleanup, err := RegisterReadSeekerInput(src.Name, src.ReadSeeker)
	if err != nil {
		return preparedSource{}, err
	}
	return preparedSource{path: path, cleanup: cleanup}, nil
}
