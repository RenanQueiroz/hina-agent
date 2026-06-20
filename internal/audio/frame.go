package audio

import (
	"encoding/binary"
	"errors"
)

// AudioFrameHeaderLen is the size in bytes of the binary header prefixed to each
// PCM frame sent over the audio datachannel. Layout (little-endian, matching the
// browser AudioWorklet's DataView reads):
//
//	[0:4]   uint32 seq         monotonic frame sequence (gap/reorder detection)
//	[4:8]   uint32 epoch       playback epoch; bumped on every (re)start so the
//	                           client can drop frames from a superseded playback
//	[8:12]  uint32 samples     mono s16 sample count in the body
//	[12:20] int64  sendMicros  server monotonic microseconds since session start
//	[20:..] s16le  pcm         samples*2 bytes
//
// epoch + seq together make the unreliable, unordered audio channel safe: a
// frame is played only if its epoch is the active one and its seq advances, so
// reordered or post-interrupt frames are dropped rather than glitching playback
// or lying about the barge-in truncation point. sendMicros uses a single clock
// (the server's) so the worklet can echo the play-cursor frame's timestamp and
// the server derives a round-trip latency without cross-host clock sync.
const AudioFrameHeaderLen = 20

// ErrShortAudioFrame is returned when a datachannel message is too short or its
// declared sample count disagrees with its body length.
var ErrShortAudioFrame = errors.New("audio: short or malformed audio frame")

// AudioFrame holds the decoded fields of one audio-datachannel message.
type AudioFrame struct {
	Seq        uint32
	Epoch      uint32
	Samples    uint32
	SendMicros int64
	PCM        []byte // s16le, aliased into the source message (not copied)
}

// EncodeAudioFrame writes the header followed by samples (converted to s16le)
// into dst and returns the number of bytes written. dst must be at least
// AudioFrameHeaderLen + len(samples)*2 bytes; reuse one buffer across the pacer
// loop to avoid per-frame allocation.
func EncodeAudioFrame(dst []byte, seq, epoch uint32, sendMicros int64, samples []float32) int {
	binary.LittleEndian.PutUint32(dst[0:], seq)
	binary.LittleEndian.PutUint32(dst[4:], epoch)
	binary.LittleEndian.PutUint32(dst[8:], uint32(len(samples)))
	binary.LittleEndian.PutUint64(dst[12:], uint64(sendMicros))
	FloatToS16LE(dst[AudioFrameHeaderLen:], samples)
	return AudioFrameHeaderLen + len(samples)*2
}

// EncodedAudioFrameLen is the message size EncodeAudioFrame produces for a frame
// of n samples.
func EncodedAudioFrameLen(n int) int { return AudioFrameHeaderLen + n*2 }

// DecodeAudioFrame parses a datachannel audio message. It validates that the
// body length matches the declared sample count so a truncated frame is rejected
// rather than silently played as noise. The returned PCM aliases msg.
func DecodeAudioFrame(msg []byte) (AudioFrame, error) {
	if len(msg) < AudioFrameHeaderLen {
		return AudioFrame{}, ErrShortAudioFrame
	}
	f := AudioFrame{
		Seq:        binary.LittleEndian.Uint32(msg[0:]),
		Epoch:      binary.LittleEndian.Uint32(msg[4:]),
		Samples:    binary.LittleEndian.Uint32(msg[8:]),
		SendMicros: int64(binary.LittleEndian.Uint64(msg[12:])),
		PCM:        msg[AudioFrameHeaderLen:],
	}
	if uint32(len(f.PCM)) != f.Samples*2 {
		return AudioFrame{}, ErrShortAudioFrame
	}
	return f, nil
}
