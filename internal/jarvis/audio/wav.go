package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// WriteWAV writes raw 16-bit signed little-endian PCM mono samples to a WAV
// file at the given path with the given sample rate. We don't pull in a
// separate WAV library because the format is small enough to inline and our
// pipeline only ever speaks 16-bit mono.
func WriteWAV(path string, pcm []byte, sampleRate int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	const (
		bitsPerSample = 16
		numChannels   = 1
	)
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataLen := uint32(len(pcm))

	if _, err := f.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(36+dataLen)); err != nil {
		return err
	}
	if _, err := f.Write([]byte("WAVEfmt ")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(16)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(numChannels)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(byteRate)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(blockAlign)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(bitsPerSample)); err != nil {
		return err
	}
	if _, err := f.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, dataLen); err != nil {
		return err
	}
	if _, err := f.Write(pcm); err != nil {
		return err
	}
	return nil
}

// ReadAllPCM reads up to maxBytes from r into a slice and returns it. Used
// for one-shot capture from an ffmpeg pipe.
func ReadAllPCM(r io.Reader, maxBytes int) ([]byte, error) {
	buf := make([]byte, 0, maxBytes)
	tmp := make([]byte, 8192)
	for len(buf) < maxBytes {
		n, err := r.Read(tmp)
		if n > 0 {
			remain := maxBytes - len(buf)
			if n > remain {
				n = remain
			}
			buf = append(buf, tmp[:n]...)
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, fmt.Errorf("read pcm: %w", err)
		}
	}
	return buf, nil
}
