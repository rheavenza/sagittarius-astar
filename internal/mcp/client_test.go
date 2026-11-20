package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeServer wires a Client to in-memory pipes and runs handler for every
// request line the client writes. handler may write any lines back.
func fakeServer(t *testing.T, handler func(req map[string]any, out io.Writer)) *Client {
	t.Helper()
	clientToServerR, clientToServerW := io.Pipe()
	serverToClientR, serverToClientW := io.Pipe()

	srv := &Server{Name: "fake", stdin: clientToServerW, stdout: serverToClientR}
	go func() {
		defer serverToClientW.Close()
		sc := bufio.NewScanner(clientToServerR)
		for sc.Scan() {
			var req map[string]any
			if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
				continue
			}
			handler(req, serverToClientW)
		}
	}()
	c := NewClient(srv)
	t.Cleanup(c.Close)
	return c
}

func TestServerInitiatedRequestDoesNotHijackResponse(t *testing.T) {
	pingAnswered := make(chan map[string]any, 1)

	c := fakeServer(t, func(req map[string]any, out io.Writer) {
		if req["method"] == nil {
			// This is the client's reply to our ping.
			pingAnswered <- req
			return
		}
		if req["method"] == "tools/list" {
			// Before answering, the server sends its own request with the SAME id.
			io.WriteString(out, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")
			io.WriteString(out, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"real"}]}}`+"\n")
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := c.Call(ctx, "tools/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "real") {
		t.Fatalf("response was hijacked by the server's ping: %s", raw)
	}

	select {
	case reply := <-pingAnswered:
		if reply["result"] == nil {
			t.Errorf("ping should be answered with a result: %v", reply)
		}
	case <-time.After(2 * time.Second):
		t.Error("server ping was never answered")
	}
}

func TestExposedName(t *testing.T) {
	if got := ExposedName("filesystem", "write_file"); got != "mcp__filesystem__write_file" {
		t.Errorf("got %q", got)
	}
	if got := ExposedName("my server", "do.thing"); got != "mcp__my_server__do_thing" {
		t.Errorf("unsafe characters not sanitized: %q", got)
	}
	long := ExposedName(strings.Repeat("s", 50), strings.Repeat("t", 50))
	if len(long) > maxToolNameLen {
		t.Errorf("name too long: %d", len(long))
	}
}
