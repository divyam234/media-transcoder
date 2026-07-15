package server

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

type mp4Box struct {
	typ         string
	start       int
	headerSize  int
	payloadFrom int
	end         int
}

var mp4ContainerBoxes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"moof": true, "traf": true, "mvex": true, "edts": true, "dinf": true,
}

func walkMP4Boxes(data []byte, from, to int, visit func(mp4Box) bool) error {
	for off := from; off < to; {
		if off+8 > to {
			return fmt.Errorf("truncated MP4 box header at %d", off)
		}
		size := uint64(binary.BigEndian.Uint32(data[off : off+4]))
		typ := string(data[off+4 : off+8])
		header := 8
		switch size {
		case 0:
			size = uint64(to - off)
		case 1:
			if off+16 > to {
				return fmt.Errorf("truncated extended MP4 box header at %d", off)
			}
			size = binary.BigEndian.Uint64(data[off+8 : off+16])
			header = 16
		}
		if size < uint64(header) || size > uint64(to-off) {
			return fmt.Errorf("invalid MP4 box %q size %d at %d", typ, size, off)
		}
		box := mp4Box{typ: typ, start: off, headerSize: header, payloadFrom: off + header, end: off + int(size)}
		if !visit(box) {
			return nil
		}
		if mp4ContainerBoxes[typ] {
			if err := walkMP4Boxes(data, box.payloadFrom, box.end, visit); err != nil {
				return err
			}
		}
		off = box.end
	}
	return nil
}

func findStructuredMP4Box(data []byte, typ string) (mp4Box, error) {
	var found mp4Box
	err := walkMP4Boxes(data, 0, len(data), func(box mp4Box) bool {
		if box.typ == typ {
			found = box
			return false
		}
		return true
	})
	if err != nil {
		return mp4Box{}, err
	}
	if found.typ == "" {
		return mp4Box{}, fmt.Errorf("MP4 %s box not found", typ)
	}
	return found, nil
}

func patchFMP4DecodeTime(initPath, mediaPath string, startSeconds float64) error {
	if startSeconds <= 0 {
		return nil
	}
	initData, err := os.ReadFile(initPath)
	if err != nil {
		return err
	}
	timescale, err := mp4TrackTimescale(initData)
	if err != nil {
		return err
	}
	mediaData, err := os.ReadFile(mediaPath)
	if err != nil {
		return err
	}
	if err := setTFDTBaseDecodeTime(mediaData, uint64(math.Round(startSeconds*float64(timescale)))); err != nil {
		return err
	}
	return os.WriteFile(mediaPath, mediaData, 0o644)
}

func mp4TrackTimescale(data []byte) (uint32, error) {
	box, err := findStructuredMP4Box(data, "mdhd")
	if err != nil {
		return 0, err
	}
	if box.payloadFrom+4 > box.end {
		return 0, fmt.Errorf("truncated mdhd box")
	}
	version := data[box.payloadFrom]
	off := box.payloadFrom + 4
	if version == 0 {
		off += 8
	} else if version == 1 {
		off += 16
	} else {
		return 0, fmt.Errorf("unsupported mdhd version %d", version)
	}
	if off+4 > box.end {
		return 0, fmt.Errorf("truncated mdhd timescale")
	}
	timescale := binary.BigEndian.Uint32(data[off : off+4])
	if timescale == 0 {
		return 0, fmt.Errorf("invalid zero MP4 track timescale")
	}
	return timescale, nil
}

func setTFDTBaseDecodeTime(data []byte, decodeTime uint64) error {
	box, err := findStructuredMP4Box(data, "tfdt")
	if err != nil {
		return err
	}
	if box.payloadFrom+4 > box.end {
		return fmt.Errorf("truncated tfdt box")
	}
	version := data[box.payloadFrom]
	off := box.payloadFrom + 4
	switch version {
	case 0:
		if decodeTime > math.MaxUint32 || off+4 > box.end {
			return fmt.Errorf("tfdt version 0 decode time overflow or truncation")
		}
		binary.BigEndian.PutUint32(data[off:off+4], uint32(decodeTime))
	case 1:
		if off+8 > box.end {
			return fmt.Errorf("truncated tfdt box")
		}
		binary.BigEndian.PutUint64(data[off:off+8], decodeTime)
	default:
		return fmt.Errorf("unsupported tfdt version %d", version)
	}
	return nil
}

func patchFMP4SampleDuration(mediaPath string, targetDuration uint64) error {
	data, err := os.ReadFile(mediaPath)
	if err != nil {
		return err
	}
	box, err := findStructuredMP4Box(data, "trun")
	if err != nil {
		return err
	}
	if box.payloadFrom+8 > box.end {
		return fmt.Errorf("truncated trun header")
	}
	flags := uint32(data[box.payloadFrom+1])<<16 | uint32(data[box.payloadFrom+2])<<8 | uint32(data[box.payloadFrom+3])
	if flags&0x100 == 0 {
		return fmt.Errorf("trun does not contain per-sample durations")
	}
	count := int(binary.BigEndian.Uint32(data[box.payloadFrom+4 : box.payloadFrom+8]))
	cursor := box.payloadFrom + 8
	if flags&0x001 != 0 {
		cursor += 4
	}
	if flags&0x004 != 0 {
		cursor += 4
	}
	entrySize := 0
	if flags&0x100 != 0 {
		entrySize += 4
	}
	if flags&0x200 != 0 {
		entrySize += 4
	}
	if flags&0x400 != 0 {
		entrySize += 4
	}
	if flags&0x800 != 0 {
		entrySize += 4
	}
	if count <= 0 || entrySize <= 0 || cursor+count*entrySize > box.end {
		return fmt.Errorf("truncated trun sample table")
	}
	durationOffsets := make([]int, count)
	var total uint64
	for n := 0; n < count; n++ {
		durationOffsets[n] = cursor
		total += uint64(binary.BigEndian.Uint32(data[cursor : cursor+4]))
		cursor += entrySize
	}
	if total < targetDuration {
		delta := targetDuration - total
		off := durationOffsets[len(durationOffsets)-1]
		cur := uint64(binary.BigEndian.Uint32(data[off : off+4]))
		if cur+delta > math.MaxUint32 {
			return fmt.Errorf("trun duration adjustment overflow")
		}
		binary.BigEndian.PutUint32(data[off:off+4], uint32(cur+delta))
	} else if total > targetDuration {
		remaining := total - targetDuration
		for n := len(durationOffsets) - 1; n >= 0 && remaining > 0; n-- {
			off := durationOffsets[n]
			cur := uint64(binary.BigEndian.Uint32(data[off : off+4]))
			if cur <= 1 {
				continue
			}
			reduce := remaining
			if reduce > cur-1 {
				reduce = cur - 1
			}
			binary.BigEndian.PutUint32(data[off:off+4], uint32(cur-reduce))
			remaining -= reduce
		}
		if remaining > 0 {
			return fmt.Errorf("cannot reduce trun durations to requested total")
		}
	}
	return os.WriteFile(mediaPath, data, 0o644)
}

func validateFMP4Init(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 16 {
		return false
	}
	_, moovErr := findStructuredMP4Box(data, "moov")
	_, mdhdErr := findStructuredMP4Box(data, "mdhd")
	return moovErr == nil && mdhdErr == nil
}

func validateFMP4Media(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 24 {
		return false
	}
	_, moofErr := findStructuredMP4Box(data, "moof")
	_, tfdtErr := findStructuredMP4Box(data, "tfdt")
	_, trunErr := findStructuredMP4Box(data, "trun")
	_, mdatErr := findStructuredMP4Box(data, "mdat")
	return moofErr == nil && tfdtErr == nil && trunErr == nil && mdatErr == nil
}
