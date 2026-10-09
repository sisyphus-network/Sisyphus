package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/sisyphus-network/Sisyphus/packages/s3"
)

// objectStore is a real object store, SeaweedFS with its S3 gateway in a
// container, started the first time a test wants it.
var objectStore struct {
	once     sync.Once
	endpoint string
	err      error
}

const (
	objectStoreAccess = "sisyphus-test"
	objectStoreSecret = "sisyphus-test-secret"
)

// bucketFor starts the object store if need be, makes a bucket in it for a
// test, and returns where it is and the bucket's name. A machine with no
// Docker skips the test, unless told that it must have it.
func bucketFor(t *testing.T) (endpoint, bucket string) {
	t.Helper()
	objectStore.once.Do(func() {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			objectStore.err = err
			return
		}
		port := lis.Addr().(*net.TCPAddr).Port
		lis.Close()
		name := fmt.Sprintf("sisyphus-daemon-s3-test-%d", os.Getpid())
		out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:8333", port),
			"-e", "AWS_ACCESS_KEY_ID="+objectStoreAccess, "-e", "AWS_SECRET_ACCESS_KEY="+objectStoreSecret,
			"chrislusf/seaweedfs:latest", "server", "-s3", "-dir=/data").CombinedOutput()
		if err != nil {
			objectStore.err = fmt.Errorf("start the object store: %v: %s", err, bytes.TrimSpace(out))
			return
		}
		stopAtExit = append(stopAtExit, func() { exec.Command("docker", "rm", "-f", name).Run() })
		at := fmt.Sprintf("http://127.0.0.1:%d", port)
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if res, err := http.Get(at); err == nil {
				res.Body.Close()
				objectStore.endpoint = at
				return
			}
		}
		objectStore.err = errors.New("the object store did not start in time")
	})
	if objectStore.err != nil {
		if os.Getenv("SISYPHUS_REQUIRE_DOCKER") != "" {
			t.Fatalf("no object store to test against, and SISYPHUS_REQUIRE_DOCKER is set: %v", objectStore.err)
		}
		t.Skipf("no object store to test against: %v", objectStore.err)
	}
	api, err := minio.New(strings.TrimPrefix(objectStore.endpoint, "http://"), &minio.Options{Creds: credentials.NewStaticV4(objectStoreAccess, objectStoreSecret, "")})
	if err != nil {
		t.Fatal(err)
	}
	bucket = fmt.Sprintf("test-%d", time.Now().UnixNano())
	// The gateway answers before it is ready to make buckets.
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if err = api.MakeBucket(context.Background(), bucket, minio.MakeBucketOptions{}); err == nil {
			return objectStore.endpoint, bucket
		}
		if time.Now().After(deadline) {
			t.Fatalf("make bucket: %v", err)
		}
	}
}

func TestACoordinatorKeepsItsStoredDataInABucket(t *testing.T) {
	endpoint, bucket := bucketFor(t)
	dataDir := t.TempDir()
	keys := filepath.Join(t.TempDir(), "keys")
	os.WriteFile(keys, []byte(objectStoreAccess+"\n"+objectStoreSecret+"\n"), 0o600)
	addr := freeAddr(t)
	args := []string{"--data-dir", dataDir, "--listen", addr, "--name", "rig", "--slots", "2",
		"--s3-endpoint", endpoint, "--s3-bucket", bucket, "--s3-prefix", "pool-one", "--s3-credentials", keys}
	stop := startDaemon(t, args...)
	waitForOutput(t, "rig", "nodes", "--addr", addr)

	input := filepath.Join(t.TempDir(), "words.txt")
	os.WriteFile(input, []byte(strings.Repeat("the boulder rolls and the boulder rolls again\n", 2000)), 0o600)
	cid := strings.TrimSpace(mustCLI(t, "blob", "put", "--addr", addr, input))
	out := mustCLI(t, "job", "submit", "--addr", addr, "--workload", "wordcount", "--params", `{"input":"`+cid+`"}`, "--tasks", "2")
	if !strings.Contains(out, `"words":16000`) {
		t.Fatalf("a job over data kept in a bucket:\n%s", out)
	}
	// The data is in the bucket, under the prefix, and nowhere on this
	// machine but for the record of what is kept.
	client, err := s3.Open(context.Background(), s3.Config{Endpoint: endpoint, Bucket: bucket, Prefix: "pool-one", AccessKey: objectStoreAccess, SecretKey: objectStoreSecret})
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	client.List(context.Background(), func(name string, _ int64) bool {
		if strings.HasPrefix(name, "blocks/") {
			blocks++
		}
		return true
	})
	if blocks < 2 {
		t.Errorf("the bucket holds %d blocks", blocks)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "blobs")); !os.IsNotExist(err) {
		t.Errorf("a store on this machine's disk beside the bucket: %v", err)
	}
	// It is all still there for a coordinator started again.
	stop()
	startDaemon(t, args...)
	waitForOutput(t, "rig", "nodes", "--addr", addr)
	if got := mustCLI(t, "blob", "stat", "--addr", addr, cid); !strings.Contains(got, "92000") {
		t.Errorf("the file after a restart: %s", got)
	}

	oneLine := filepath.Join(t.TempDir(), "keys")
	os.WriteFile(oneLine, []byte(objectStoreAccess+"\n"), 0o600)
	inUse := []string{"--data-dir", dataDir, "--s3-endpoint", endpoint, "--s3-bucket", bucket, "--s3-prefix", "pool-one", "--s3-credentials", keys}
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no bucket", []string{"--data-dir", t.TempDir(), "--s3-endpoint", endpoint}, "needs --s3-bucket"},
		{"with Kubo too", []string{"--data-dir", t.TempDir(), "--s3-endpoint", endpoint, "--s3-bucket", bucket, "--kubo"}, "choose one"},
		{"on a node with no pool", []string{"--data-dir", t.TempDir(), "--role", "worker", "--coordinator", "127.0.0.1:1", "--s3-endpoint", endpoint, "--s3-bucket", bucket}, "is for a node that coordinates a pool"},
		{"credentials that are not there", []string{"--data-dir", t.TempDir(), "--s3-endpoint", endpoint, "--s3-bucket", bucket, "--s3-credentials", filepath.Join(t.TempDir(), "absent")}, "read the bucket's credentials"},
		{"credentials of one line", []string{"--data-dir", t.TempDir(), "--s3-endpoint", endpoint, "--s3-bucket", bucket, "--s3-credentials", oneLine}, "the access key on its first line and the secret key on its second"},
		{"a bucket that is not there", []string{"--data-dir", t.TempDir(), "--s3-endpoint", endpoint, "--s3-bucket", "no-such-bucket-here", "--s3-credentials", keys}, "there is no bucket"},
		{"a data directory in use", inUse, "in use by another process"},
	} {
		if _, err := cli(t, append([]string{"run", "--listen", freeAddr(t)}, tt.args...)...); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("--s3-endpoint %s: %v, want %q", tt.name, err, tt.want)
		}
	}
}
