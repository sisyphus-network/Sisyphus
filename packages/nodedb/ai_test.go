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

func TestModelProfilesReportMalformedRowsAndMissingTable(t *testing.T) {
	db, _ := newDB(t)
	if _, err := db.sql.Exec(`DROP TABLE model_profiles`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TABLE model_profiles (
		profile_id TEXT, name TEXT, provider TEXT, base_url TEXT, model TEXT, api_key TEXT, is_active TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO model_profiles VALUES ('broken', 'Broken', 'ollama', '', 'm', '', 'not-an-integer')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ModelProfiles(); err == nil || !strings.Contains(err.Error(), "load model profiles") {
		t.Errorf("a malformed profile row: %v", err)
	}
	if _, err := db.sql.Exec(`DROP TABLE model_profiles`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ModelProfiles(); err == nil || !strings.Contains(err.Error(), "load model profiles") {
		t.Errorf("a missing profiles table: %v", err)
	}
}

func TestModelProfilesKeepMultipleServicesAndOnlyOneActive(t *testing.T) {
	db, _ := newDB(t)
	profiles := []ModelProfile{
		{ID: "local", Name: "Local Ollama", Config: ModelConfig{Provider: "ollama", BaseURL: "http://localhost:11434", Model: "qwen3"}, Active: true},
		{ID: "cloud", Name: "Cloud API", Config: ModelConfig{Provider: "openai", BaseURL: "https://api.example.net/v1", Model: "gpt-5", APIKey: "secret"}},
	}
	for _, p := range profiles {
		if err := db.SaveModelProfile(p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.ModelProfiles()
	if err != nil || len(got) != 2 {
		t.Fatalf("profiles = %#v, %v", got, err)
	}
	if !got[0].Active || got[1].Config.APIKey != "secret" {
		t.Fatalf("profiles lost active/key state: %#v", got)
	}
	profiles[1].Active = true
	profiles[1].Config.Model = "gpt-5-mini"
	if err := db.SaveModelProfile(profiles[1]); err != nil {
		t.Fatal(err)
	}
	got, _ = db.ModelProfiles()
	if !got[0].Active || got[0].ID != "cloud" {
		t.Fatalf("active profile = %#v", got)
	}
	active, found, err := db.ModelConfig()
	if err != nil || !found || active.Provider != "openai" || active.Model != "gpt-5-mini" || active.APIKey != "secret" {
		t.Fatalf("active config = %#v, %v, %v", active, found, err)
	}
	if err := db.DeleteModelProfile("cloud"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.ModelProfiles()
	if len(got) != 2 || !got[0].Active {
		t.Fatalf("active profile was removed: %#v", got)
	}
	if err := db.DeleteModelProfile("local"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.ModelProfiles()
	if len(got) != 1 || !got[0].Active {
		t.Fatalf("profiles after delete = %#v", got)
	}
}

func TestChatsAndWhatWasSaidInThem(t *testing.T) {
	db, file := newDB(t)
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	for _, c := range []Chat{{ID: "older", Title: "Primes below a million", Created: at}, {ID: "newer", Title: "Word counts", Created: at.Add(time.Hour)}} {
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
	if len(chats) == 2 && (chats[0].Revision != 1 || chats[1].Revision != 3) {
		t.Errorf("chat revisions = %d, %d; want 1, 3", chats[0].Revision, chats[1].Revision)
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
	if err := db.CreateChat(Chat{ID: "newer", Title: "again", Created: at}); err == nil || !strings.Contains(err.Error(), "save chat") {
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
