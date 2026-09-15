package brain

import (
	"encoding/binary"
	"math"
)

// VectorDims is the dimensionality every stored Voyage AI embedding is
// pinned to via output_dimension (embeddings.go's voyageOutputDimension) --
// independent of the underlying model, which has changed over time
// (voyage-3-lite, then voyage-4-lite) while staying at this dimension.
const VectorDims = 512

// EncodeVector serializes a float32 slice to bytes for SQLite BLOB storage.
// Each float32 is stored as 4 bytes in little-endian order.
func EncodeVector(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// DecodeVector deserializes bytes back to a float32 slice.
func DecodeVector(b []byte) []float32 {
	if len(b) == 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// CosineSimilarity computes the cosine similarity between two vectors.
// Returns a value in [-1, 1] where 1 means identical direction.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
