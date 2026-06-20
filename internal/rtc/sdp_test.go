package rtc

import "testing"

// sdpWith builds a minimal offer SDP with the given session-level and audio
// media-level direction attribute lines (empty = omitted).
func sdpWith(sessionDir, mediaDir string) string {
	s := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"t=0 0\r\n"
	if sessionDir != "" {
		s += "a=" + sessionDir + "\r\n"
	}
	s += "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=rtpmap:111 opus/48000/2\r\n"
	if mediaDir != "" {
		s += "a=" + mediaDir + "\r\n"
	}
	return s
}

func TestOfferSendingAudioCount(t *testing.T) {
	cases := []struct {
		name      string
		session   string
		media     string
		wantSends int
	}{
		{"default (no direction) is sending", "", "", 1},
		{"media sendrecv", "", "sendrecv", 1},
		{"media sendonly", "", "sendonly", 1},
		{"media recvonly is not sending", "", "recvonly", 0},
		{"media inactive is not sending", "", "inactive", 0},
		{"session recvonly inherited", "recvonly", "", 0},
		{"session inactive inherited", "inactive", "", 0},
		{"media overrides session recvonly", "recvonly", "sendrecv", 1},
		{"media recvonly overrides session sendrecv", "sendrecv", "recvonly", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, err := offerSendingAudioCount(sdpWith(c.session, c.media))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if n != c.wantSends {
				t.Fatalf("sending audio count = %d, want %d", n, c.wantSends)
			}
		})
	}
}
