package nodedb

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTheModelConfigurationIsKept(t *testing.T) {
	db, file := newDB(t)
	if cfg, found, err := db.ModelConfig(); err != nil || found || cfg != (ModelConfig{}) {
		t.Fatalf("on a new node: %+v, %v, %v", cfg, found, err)
	}
	first := ModelConfig{Provider: "ollama", Model: "qwen3:8b"}
	second := ModelConfig{Provider: "openai", BaseURL: "https://api.example.net/v1", Model: "gpt-5", APIKey: "sk-secret"}
	for _, cfg := range []ModelConfig{first, second} {
		if err := db.SetModelConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	db = reopen(t, db, file)
	if cfg, found, err := db.ModelConfig(); err != nil || !found || cfg != second {
		t.Errorf("after reopening: %+v, %v, %v", cfg, found, err)
	}
}

func TestChatsAndWhatWasSaidInThem(t *testing.T) {
	db, file := newDB(t)
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	for _, c := range []Chat{{"older", "Primes below a million", at}, {"newer", "Word counts", at.Add(time.Hour)}} {
		if err := db.CreateChat(c); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AppendChatMessages("older", []string{`{"role":"user"}`, `{"role":"assistant"}`}, at); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendChatMessages("older", []string{`{"role":"tool"}`}, at); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendChatMessages("newer", []string{`{"role":"user","content":"other"}`}, at); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, file)

	chats, err := db.Chats()
	if err != nil || len(chats) != 2 || chats[0].ID != "newer" || chats[1].Title != "Primes below a million" || !chats[1].Created.Equal(at) {
		t.Errorf("chats, newest first: %+v, %v", chats, err)
	}
	said, err := db.ChatMessages("older")
	if err != nil || !reflect.DeepEqual(said, []string{`{"role":"user"}`, `{"role":"assistant"}`, `{"role":"tool"}`}) {
		t.Errorf("what was said, in order: %v, %v", said, err)
	}
	// Forgetting a chat forgets what was said in it, and nothing else.
	if err := db.DeleteChat("older"); err != nil {
		t.Fatal(err)
	}
	if left, _ := db.ChatMessages("older"); len(left) != 0 {
		t.Errorf("messages left after their chat was deleted: %v", left)
	}
	if kept, _ := db.ChatMessages("newer"); len(kept) != 1 {
		t.Errorf("the other chat's messages: %v", kept)
	}
	// Nothing can be said in a chat that is not there, or started twice.
	if err := db.AppendChatMessages("older", []string{`{}`}, at); err == nil || !strings.Contains(err.Error(), "save chat messages") {
		t.Errorf("saying something in a deleted chat: %v", err)
	}
	if err := db.CreateChat(Chat{"newer", "again", at}); err == nil || !strings.Contains(err.Error(), "save chat") {
		t.Errorf("starting a chat twice: %v", err)
	}
}

func TestAIStateFailuresAreReported(t *testing.T) {
	db, _ := newDB(t)
	loosen(t, db, "chats", "chat_id, title, created_at_ns")
	if _, err := db.Chats(); err == nil || !strings.Contains(err.Error(), "load chats") {
		t.Errorf("with a damaged chats table: %v", err)
	}
	loosen(t, db, "chat_messages", "chat_id DEFAULT 'c', seq, at_ns, message")
	if _, err := db.ChatMessages("c"); err == nil || !strings.Contains(err.Error(), "load chat messages") {
		t.Errorf("with a damaged messages table: %v", err)
	}
	db.Close()
	if _, _, err := db.ModelConfig(); err == nil || !strings.Contains(err.Error(), "load model configuration") {
		t.Errorf("ModelConfig on a closed database: %v", err)
	}
	for what, err := range map[string]error{
		"save model configuration": db.SetModelConfig(ModelConfig{}),
		"delete chat":              db.DeleteChat("c"),
	} {
		if err == nil || !strings.Contains(err.Error(), what) {
			t.Errorf("%s on a closed database: %v", what, err)
		}
	}
}
