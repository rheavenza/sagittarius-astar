package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

func TestPrivateURL(t *testing.T) {
	ctx := context.Background()
	private := []string{
		"http://localhost:8080/x", "http://LOCALHOST./", "http://127.0.0.1/", "http://[::1]:3000/",
		"http://10.1.2.3/", "http://172.16.0.1/", "http://192.168.1.1/admin", "http://169.254.169.254/latest/meta-data",
		"http://100.101.102.103/", "http://[fd00::1]/", "http://0.0.0.0:80/", "http://[::ffff:127.0.0.1]/",
		"http://printer.local/", "http://nas.tail1234.ts.net/", "http://db.internal/", "http://app.localhost/",
	}
	public := []string{"https://1.1.1.1/", "http://8.8.8.8/", "https://[2606:4700:4700::1111]/", "not a url", ""}
	for _, u := range private {
		if ok, why := PrivateURL(ctx, u); !ok || why == "" {
			t.Errorf("should be private: %q", u)
		}
	}
	for _, u := range public {
		if ok, why := PrivateURL(ctx, u); ok {
			t.Errorf("should not be private: %q (%s)", u, why)
		}
	}
}

func fetch(ctx context.Context, u string) (string, error) {
	out, _, err := Execute(ctx, llm.ToolCall{Name: "web_fetch", Args: map[string]any{"url": u}})
	return out, err
}

func TestWebFetchNeedsApprovalForPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><p>internal dashboard</p></body></html>"))
	}))
	defer srv.Close()

	if _, err := fetch(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("unapproved fetch to %s should be refused, got %v", srv.URL, err)
	}
	out, err := fetch(WithPrivateNetwork(context.Background()), srv.URL)
	if err != nil || !strings.Contains(out, "internal dashboard") {
		t.Fatalf("approved fetch: out=%q err=%v", out, err)
	}
	if _, err := fetch(context.Background(), "file:///etc/passwd"); err == nil || !strings.Contains(err.Error(), "only http") {
		t.Fatalf("file:// should be rejected, got %v", err)
	}
}

func TestGuardedClientRefusesRedirectIntoPrivateNetwork(t *testing.T) {
	target, _ := url.Parse("http://169.254.169.254/latest/meta-data")
	req := &http.Request{URL: target}
	req = req.WithContext(context.Background())
	via := []*http.Request{{URL: &url.URL{Scheme: "https", Host: "1.1.1.1"}}}

	if err := guardedClient(context.Background(), time.Second).CheckRedirect(req, via); err == nil {
		t.Fatal("redirect into a private address should be refused")
	}
	if err := guardedClient(WithPrivateNetwork(context.Background()), time.Second).CheckRedirect(req, via); err != nil {
		t.Fatalf("approved private fetch may follow private redirects: %v", err)
	}
}

func TestWebFetchIsCancelledWithContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(WithPrivateNetwork(context.Background()))
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := fetch(ctx, srv.URL)
	if err == nil {
		t.Fatal("cancelled fetch should fail")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("fetch took %v after cancellation; it should stop right away", d)
	}
}
