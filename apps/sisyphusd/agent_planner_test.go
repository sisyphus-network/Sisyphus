package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// An agent hands the node's planner a question about a stored file. The
// planner's model is told of the file as the desktop app would tell it,
// and the node keeps the file for as long as the conversation.
func TestAnAgentAsksThePlannerAboutAFileAndTheNodeKeepsItForTheConversation(t *testing.T) {
	served := newModel(t, says("It is about a boulder."), says("And a hill."))
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "1", "--api-listen", apiAddr)
	poolAsSeenBy(t, desktop(t, apiAddr))
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	mustCLI(t, "model", "set", "--data-dir", dataDir, "--url", served.URL, "--model", "test-model")
	session := agent(t, dataDir, apiAddr, "--addr", addr, "--files-under", "/")

	stored, text, failed := use(t, session, "store_file", map[string]any{"path": writeFile(t, "the boulder and the hill"), "name": "myth.txt", "private": true})
	if failed {
		t.Fatalf("store_file: %s", text)
	}
	file := stored.(map[string]any)["cid"].(string)

	said, text, failed := use(t, session, "ask_planner", map[string]any{"question": "What is this about?", "files": []string{file}})
	if failed || said.(map[string]any)["answer"] != "It is about a boulder." {
		t.Fatalf("ask_planner with a file: %s", text)
	}
	chat := said.(map[string]any)["chat_id"].(string)
	// What the model was sent names the file, and says it is private.
	served.mu.Lock()
	asked, _ := json.Marshal(served.asked[0]["messages"])
	served.mu.Unlock()
	if want := `[Sisyphus attachments]\n- \"myth.txt\" | cid:` + file + ` | image:false | private:true`; !strings.Contains(string(asked), want) {
		t.Errorf("the model was sent %s, without %s", asked, want)
	}
	// The conversation keeps the file, beside the user who stored it.
	if _, text, failed := use(t, session, "list_pins", nil); failed || !strings.Contains(text, "chat:"+chat) {
		t.Errorf("list_pins after asking about a file: %s", text)
	}
	// Carried on, the conversation needs no telling of the file again.
	if said, text, failed := use(t, session, "ask_planner", map[string]any{"question": "And what else?", "chat_id": chat}); failed || said.(map[string]any)["answer"] != "And a hill." {
		t.Errorf("ask_planner, carried on: %s", text)
	}

	// A file the node has not stored cannot be asked about.
	if _, text, failed := use(t, session, "ask_planner", map[string]any{"question": "And this?", "files": []string{"bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"}}); !failed || !strings.Contains(text, "is not a file this node has stored") {
		t.Errorf("ask_planner about a file that is not stored: %s", text)
	}

	// Forgetting the conversation lets the file go for it, and not for the
	// user who stored it.
	if _, text, failed := use(t, session, "delete_chat", map[string]any{"chat_id": chat}); failed {
		t.Fatalf("delete_chat: %s", text)
	}
	waitFor(t, func() bool {
		_, text, _ := use(t, session, "list_pins", nil)
		return !strings.Contains(text, "chat:"+chat) && strings.Contains(text, file)
	})
}
