package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sseServer returns an httptest server that writes the given SSE frames then
// closes the connection.
func sseServer(frames ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
		}
	}))
}

// collect drains a delta channel into the accumulated text and terminal flags.
func collect(ch <-chan Delta) (text string, done, errd bool) {
	for d := range ch {
		switch {
		case d.Err != nil:
			errd = true
		case d.Done:
			done = true
		default:
			text += d.Text
		}
	}
	return
}

func streamOnce(t *testing.T, baseURL string) (string, bool, bool) {
	t.Helper()
	p := NewOpenAICompatProvider(baseURL, "k", "m")
	ch, err := p.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return collect(ch)
}

// TestCompatTruncatedStreamErrors proves a clean EOF before [DONE]/finish_reason
// is an error, not a successful completion — so truncated backend output can't
// be committed as canonical history.
func TestCompatTruncatedStreamErrors(t *testing.T) {
	srv := sseServer(
		`data: {"choices":[{"delta":{"content":"par"}}]}`+"\n\n",
		`data: {"choices":[{"delta":{"content":"tial"}}]}`+"\n\n",
	)
	defer srv.Close()
	_, done, errd := streamOnce(t, srv.URL)
	if !errd {
		t.Fatal("truncated stream (no [DONE]/finish_reason) must yield an error")
	}
	if done {
		t.Fatal("truncated stream must not signal Done")
	}
}

// TestCompatDoneCompletes proves a proper [DONE]-terminated stream completes.
func TestCompatDoneCompletes(t *testing.T) {
	srv := sseServer(
		`data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n",
		"data: [DONE]\n\n",
	)
	defer srv.Close()
	text, done, errd := streamOnce(t, srv.URL)
	if errd || !done || text != "hi" {
		t.Fatalf("text=%q done=%v err=%v, want \"hi\",true,false", text, done, errd)
	}
}

// TestCompatFinishReasonCompletes proves a finish_reason (without an explicit
// [DONE]) is accepted as a terminal marker.
func TestCompatFinishReasonCompletes(t *testing.T) {
	srv := sseServer(`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n")
	defer srv.Close()
	text, done, errd := streamOnce(t, srv.URL)
	if errd || !done || text != "hi" {
		t.Fatalf("text=%q done=%v err=%v, want \"hi\",true,false", text, done, errd)
	}
}

// TestCompatErrorFrameErrors proves a streamed error frame is propagated as a
// hard failure (never Done).
func TestCompatErrorFrameErrors(t *testing.T) {
	srv := sseServer(`data: {"error":{"message":"backend boom"}}` + "\n\n")
	defer srv.Close()
	_, done, errd := streamOnce(t, srv.URL)
	if !errd {
		t.Fatal("error frame must yield an error")
	}
	if done {
		t.Fatal("error frame must not signal Done")
	}
}

// TestCompatMalformedFrameThenDoneErrors proves a malformed data frame is a hard
// error even when followed by [DONE] — it must never be skipped and then
// "completed".
func TestCompatMalformedFrameThenDoneErrors(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
	}{
		{"not json", "data: not-json\n\n"},
		{"error as string", `data: {"error":"boom"}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := sseServer(tc.frame, "data: [DONE]\n\n")
			defer srv.Close()
			_, done, errd := streamOnce(t, srv.URL)
			if !errd {
				t.Fatal("malformed data frame must yield an error")
			}
			if done {
				t.Fatal("malformed data frame must not be followed by a successful Done")
			}
		})
	}
}
