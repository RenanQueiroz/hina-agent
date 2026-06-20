package audio

import "encoding/binary"

// FloatToS16LE writes src (float32 samples nominally in [-1,1]) into dst as
// little-endian signed 16-bit PCM. dst must be at least len(src)*2 bytes; it
// writes exactly len(src)*2 bytes and returns that count. Samples are clamped to
// [-1,1] before scaling so out-of-range values can't wrap to the opposite-sign
// extreme (a classic clipping artifact); +1.0 maps to 32767 (not 32768, which
// would overflow int16).
func FloatToS16LE(dst []byte, src []float32) int {
	for i, s := range src {
		if s > 1 {
			s = 1
		} else if s < -1 {
			s = -1
		}
		v := int32(s * 32767)
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(int16(v)))
	}
	return len(src) * 2
}

// S16LEToFloat32 writes the little-endian s16 PCM in src into dst as float32 in
// [-1,1]. dst must hold at least len(src)/2 samples; it returns the number of
// samples written. A trailing odd byte (a half sample) is ignored.
func S16LEToFloat32(dst []float32, src []byte) int {
	n := len(src) / 2
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(src[i*2:]))
		dst[i] = float32(v) / 32768
	}
	return n
}
