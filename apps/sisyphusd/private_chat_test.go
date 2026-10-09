package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodepb "github.com/sisyphus-network/Sisyphus/packages/protocol/sisyphus/node/v1"
	"github.com/sisyphus-network/Sisyphus/packages/sealed"
)

// Exercise the real daemon, API, worker and sealed store. Only the model
// service is scripted, deliberately omitting/refusing the private flag.
func TestPrivateChatAttachmentRunsEncryptedWorkAcrossTurns(t *testing.T) {
	dataDir := t.TempDir()
	addr, apiAddr := freeAddr(t), freeAddr(t)
	startDaemon(t, "--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "2", "--api-listen", apiAddr)
	client := desktop(t, apiAddr)
	poolAsSeenBy(t, client)
	ctx, cancel := context.WithTimeout(tokenOf(t, dataDir), 20*time.Second)
	defer cancel()
	const text = "the quick brown fox jumps over the lazy dog and the fox sleeps"
	file := storeThroughDesktop(t, ctx, client, "secret.txt", text, true)
	key, err := sealingKey(filepath.Join(dataDir, "private.key"))
	if err != nil {
		t.Fatal(err)
	}
	checkSealed := func(cid, expected string) {
		t.Helper()
		held := []byte(mustCLI(t, "blob", "get", "--addr", addr, cid))
		if bytes.Equal(held, []byte(expected)) {
			t.Fatal("pool holds plaintext")
		}
		reader, err := sealed.Open(key, bytes.NewReader(held), uint64(len(held)))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := io.ReadAll(reader)
		if err != nil || string(plain) != expected {
			t.Fatalf("stored ciphertext cannot be authenticated/decrypted: %v", err)
		}
	}
	arguments := fmt.Sprintf(`{"workload":"wordcount","params":{"input":%q},"private":false}`, file.GetCid())
	served := newModel(t, wants("run_job", arguments), says("counted"), wants("run_job", arguments), says("counted again"))
	if _, err := client.SetModelConfig(ctx, &nodepb.SetModelConfigRequest{Provider: "ollama", BaseUrl: served.URL, Model: "test-model"}); err != nil {
		t.Fatal(err)
	}

	chatID := ""
	question := fmt.Sprintf("Count these words.\n\n[Sisyphus attachments]\n- \"secret.txt\" | cid:%s | image:false | private:true", file.GetCid())
	for turn, question := range []string{question, "Count them again."} {
		req := &nodepb.AskRequest{ChatId: chatID, Text: question}
		if turn == 0 {
			req.AttachmentCids = []string{file.GetCid()}
		}
		stream, err := client.Ask(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		gotResult, gotDone := false, false
		for {
			event, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			chatID = event.GetChatId()
			if event.GetKind() == "result" {
				gotResult = strings.Contains(event.GetText(), `"state":"succeeded"`)
			}
			if event.GetKind() == "done" {
				gotDone = true
			}
		}
		if !gotResult || !gotDone || chatID == "" {
			t.Fatal("private computation did not finish through the planner")
		}
	}
	jobs, err := client.ListJobs(ctx, &nodepb.ListJobsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.GetJobs()) != 2 {
		t.Fatalf("got %d jobs, want two turns", len(jobs.GetJobs()))
	}
	for _, job := range jobs.GetJobs() {
		if !job.GetPrivate() || job.GetState() != nodepb.JobState_JOB_STATE_SUCCEEDED || len(job.GetOutputBlobs()) != 1 {
			t.Fatalf("planner job was not private and successful: %v", job)
		}
		cid := job.GetOutputBlobs()[0]
		counts, err := fetchThroughDesktop(ctx, client, cid)
		if err != nil || !strings.HasPrefix(counts, "3\tthe\n2\tfox\n") {
			t.Fatalf("private result cannot be read: %q, %v", counts, err)
		}
		checkSealed(cid, counts)
	}
	checkSealed(file.GetCid(), text)
	if _, err := client.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: file.GetCid()}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("chat attachment removal should be protected: %v", err)
	}
	if _, err := client.DeleteChat(ctx, &nodepb.DeleteChatRequest{ChatId: chatID}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RemoveFile(ctx, &nodepb.RemoveFileRequest{Cid: file.GetCid()}); err != nil {
		t.Fatalf("deleted chat still retains its attachment: %v", err)
	}
}
