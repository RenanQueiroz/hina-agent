package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go/v3/option"
)

// closeTrackingRT wraps a RoundTripper and flags when the response body it
// returns is closed, so a test can prove the SDK stream's body is released.
type closeTrackingRT struct {
	inner  http.RoundTripper
	closed *atomic.Bool
}

func (t closeTrackingRT) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(r)
	if resp != nil && resp.Body != nil {
		resp.Body = closeTracker{ReadCloser: resp.Body, closed: t.closed}
	}
	return resp, err
}

type closeTracker struct {
	io.ReadCloser
	closed *atomic.Bool
}

func (c closeTracker) Close() error {
	c.closed.Store(true)
	return c.ReadCloser.Close()
}

// TestOpenAIResponsesClosesStream proves the Responses provider releases the SDK
// stream's HTTP response body on normal completion (the same deferred Close also
// covers the error and ctx-cancel exit paths), preventing connection leaks.
func TestOpenAIResponsesClosesStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// One real delta then end-of-stream; enough for Next() to finish.
		_, _ = io.WriteString(w, "event: response.output_text.delta\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"hi"}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	var closed atomic.Bool
	client := &http.Client{Transport: closeTrackingRT{inner: http.DefaultTransport, closed: &closed}}
	p := newOpenAIResponsesProvider("m",
		option.WithBaseURL(srv.URL),
		option.WithAPIKey("k"),
		option.WithHTTPClient(client),
	)

	ch, err := p.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	for range ch { // drain to completion
	}
	if !closed.Load() {
		t.Fatal("Responses provider did not close the SDK stream body (connection leak)")
	}
}
