package audio

import (
	"encoding/binary"
	"fmt"
	"math"
)

// RMS computes the root-mean-square amplitude of 16-bit signed
// little-endian mono PCM samples in chunk. Used both by the desk-mic
// listener's amplitude VAD (segmenting speech from silence chunk-by-chunk
// as audio streams in) and by the LMD push-to-talk path (a whole-clip
// silence gate run once, before whisper is ever invoked - see
// internal/jarvis/lmd's silenceRMSThreshold).
func RMS(chunk []byte) float64 {
	var sumSq float64
	n := len(chunk) / 2
	for i := 0; i+1 < len(chunk); i += 2 {
		s := int16(binary.LittleEndian.Uint16(chunk[i : i+2]))
		f := float64(s)
		sumSq += f * f
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(sumSq / float64(n))
}

// PCMFromWAV extracts the raw PCM payload (the "data" chunk) from a WAV
// file's bytes, skipping over "fmt ", "LIST", or any other chunk that
// precedes it. Returns an error for anything that isn't a well-formed
// RIFF/WAVE container - callers should treat a parse error as "can't
// measure this clip," never as evidence the audio itself is bad silence.
func PCMFromWAV(data []byte) ([]byte, error) {
	const headerLen = 12 // "RIFF" + size(4) + "WAVE"
	if len(data) < headerLen {
		return nil, fmt.Errorf("wav: too short for a RIFF header (%d bytes)", len(data))
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("wav: missing RIFF/WAVE magic")
	}

	pos := headerLen
	for pos+8 <= len(data) {
		chunkID := string(data[pos : pos+4])
		chunkSize := binary.LittleEndian.Uint32(data[pos+4 : pos+8])
		pos += 8

		if chunkID == "data" {
			if chunkSize > uint32(len(data)-pos) {
				// Declared size runs past what's actually present (a
				// truncated upload) - measure what we DO have rather than
				// failing outright.
				return data[pos:], nil
			}
			return data[pos : pos+int(chunkSize)], nil
		}

		advance := int(chunkSize)
		if chunkSize%2 == 1 {
			advance++ // RIFF chunks are word-aligned; odd sizes get a pad byte
		}
		if advance < 0 || pos+advance > len(data) {
			return nil, fmt.Errorf("wav: chunk %q size %d overruns buffer", chunkID, chunkSize)
		}
		pos += advance
	}
	return nil, fmt.Errorf("wav: no data chunk found")
}
