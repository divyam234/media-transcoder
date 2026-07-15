package transcoder

/*
#include <stdint.h>
*/
import "C"

import (
	"fmt"
	"io"
	"runtime/cgo"
	"strconv"
	"sync"
	"unsafe"
)

const avioPrefix = "goavio:"

type readSeekCloser interface {
	io.ReadSeeker
	Close() error
}

type avioFactory struct {
	open func() (readSeekCloser, error)
}

type avioStream struct {
	mu sync.Mutex
	r  readSeekCloser
}

type noCloseReadSeeker struct{ io.ReadSeeker }

func (noCloseReadSeeker) Close() error { return nil }

// RegisterReadSeekerInput exposes one seekable stream for a single libav call.
func RegisterReadSeekerInput(name string, r io.ReadSeeker) (string, func(), error) {
	if r == nil {
		return "", nil, fmt.Errorf("nil read seeker")
	}
	return RegisterReadSeekerFactory(name, func() (io.ReadSeekCloser, error) {
		if _, err := r.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return noCloseReadSeeker{r}, nil
	})
}

// RegisterReadSeekerFactory exposes a source factory to libav. Every
// avformat_open_input call receives an independent seekable stream.
func RegisterReadSeekerFactory(name string, open func() (io.ReadSeekCloser, error)) (input string, cleanup func(), err error) {
	if open == nil {
		return "", nil, fmt.Errorf("nil read seeker factory")
	}
	factory := cgo.NewHandle(&avioFactory{open: func() (readSeekCloser, error) { return open() }})
	return avioPrefix + strconv.FormatUint(uint64(factory), 10) + "/" + name, factory.Delete, nil
}

//export goAVIOOpen
func goAVIOOpen(raw C.uintptr_t) (result C.uintptr_t) {
	defer func() {
		if recover() != nil {
			result = 0
		}
	}()
	factory, ok := cgo.Handle(raw).Value().(*avioFactory)
	if !ok || factory == nil {
		return 0
	}
	stream, err := factory.open()
	if err != nil || stream == nil {
		return 0
	}
	return C.uintptr_t(cgo.NewHandle(&avioStream{r: stream}))
}

//export goAVIOClose
func goAVIOClose(raw C.uintptr_t) {
	defer func() { _ = recover() }()
	h := cgo.Handle(raw)
	stream, _ := h.Value().(*avioStream)
	if stream != nil && stream.r != nil {
		_ = stream.r.Close()
	}
	h.Delete()
}

//export goAVIORead
func goAVIORead(raw C.uintptr_t, buf *C.uint8_t, size C.int) (result C.int) {
	defer func() {
		if recover() != nil {
			result = C.int(-5)
		}
	}()
	if size <= 0 {
		return 0
	}
	stream, ok := cgo.Handle(raw).Value().(*avioStream)
	if !ok || stream == nil {
		return C.int(-5)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	data := unsafe.Slice((*byte)(unsafe.Pointer(buf)), int(size))
	n, err := stream.r.Read(data)
	if n > 0 {
		return C.int(n)
	}
	if err == io.EOF {
		return C.int(-541478725)
	}
	if err != nil {
		return C.int(-5)
	}
	return 0
}

//export goAVIOSeek
func goAVIOSeek(raw C.uintptr_t, offset C.int64_t, whence C.int) (result C.int64_t) {
	defer func() {
		if recover() != nil {
			result = C.int64_t(-5)
		}
	}()
	stream, ok := cgo.Handle(raw).Value().(*avioStream)
	if !ok || stream == nil {
		return C.int64_t(-5)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	const avseekSize = 0x10000
	const avseekForce = 0x20000
	w := int(whence) &^ avseekForce
	if w == avseekSize {
		current, err := stream.r.Seek(0, io.SeekCurrent)
		if err != nil {
			return C.int64_t(-5)
		}
		end, err := stream.r.Seek(0, io.SeekEnd)
		if err != nil {
			return C.int64_t(-5)
		}
		if _, err := stream.r.Seek(current, io.SeekStart); err != nil {
			return C.int64_t(-5)
		}
		return C.int64_t(end)
	}
	if w != io.SeekStart && w != io.SeekCurrent && w != io.SeekEnd {
		return C.int64_t(-22)
	}
	position, err := stream.r.Seek(int64(offset), w)
	if err != nil {
		return C.int64_t(-5)
	}
	return C.int64_t(position)
}
