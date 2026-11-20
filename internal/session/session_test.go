package session

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

func TestSaveLoadRoundTripIsAtomic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s := &Session{
		ID:        GenerateID(),
		Name:      "test",
		CreatedAt: time.Now(),
		History: []llm.Message{
			{Role: llm.RoleUser, Content: "hi"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1", Name: "x", ThoughtSignature: []byte("sig")}}},
		},
	}
	for i := 0; i < 3; i++ {
		if err := Save(s); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 2 || string(got.History[1].ToolCalls[0].ThoughtSignature) != "sig" {
		t.Fatalf("round trip lost data: %+v", got.History)
	}
	info, _ := os.Stat(Dir() + "/" + s.ID + ".json")
	if info.Mode().Perm() != 0o600 {
		t.Errorf("session file mode = %v, want 0600", info.Mode().Perm())
	}
}
