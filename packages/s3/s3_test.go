package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/sisyphus-network/Sisyphus/packages/storage"
)

// objectStore is a real object store for the tests to use: SeaweedFS with
// its S3 gateway, in a container, started once and stopped when the tests
// are over.
var objectStore struct {
	once     sync.Once
	endpoint string
	err      error
}

const (
	storeImage = "chrislusf/seaweedfs:latest"
	accessKey  = "sisyphus-test"
	secretKey  = "sisyphus-test-secret"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if objectStore.endpoint != "" {
		exec.Command("docker", "rm", "-f", containerName()).Run()
	}
	os.Exit(code)
}

func containerName() string { return fmt.Sprintf("sisyphus-s3-test-%d", os.Getpid()) }

// endpoint returns where the object store is, starting it if need be. A
// machine with no Docker skips the test, unless told that it must have it.
func endpoint(t *testing.T) string {
	t.Helper()
	objectStore.once.Do(func() {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			objectStore.err = err
			return
		}
		port := lis.Addr().(*net.TCPAddr).Port
		lis.Close()
		out, err := exec.Command("docker", "run", "-d", "--rm", "--name", containerName(), "-p", fmt.Sprintf("127.0.0.1:%d:8333", port),
			"-e", "AWS_ACCESS_KEY_ID="+accessKey, "-e", "AWS_SECRET_ACCESS_KEY="+secretKey,
			storeImage, "server", "-s3", "-dir=/data").CombinedOutput()
		if err != nil {
			objectStore.err = fmt.Errorf("start %s: %v: %s", storeImage, err, bytes.TrimSpace(out))
			return
		}
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
	return objectStore.endpoint
}

// newBucket makes a bucket of its own for a test and returns its name.
func newBucket(t *testing.T) string {
	t.Helper()
	at := endpoint(t)
	api, err := minio.New(strings.TrimPrefix(at, "http://"), &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, "")})
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("test-%d", time.Now().UnixNano())
	// The gateway answers before it is ready to make buckets.
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if err = api.MakeBucket(context.Background(), name, minio.MakeBucketOptions{}); err == nil {
			return name
		}
		if time.Now().After(deadline) {
			t.Fatalf("make bucket: %v", err)
		}
	}
}

func TestBlocksAreKeptInARealObjectStore(t *testing.T) {
	ctx := context.Background()
	bucket := newBucket(t)
	client, err := Open(ctx, Config{Endpoint: endpoint(t), Bucket: bucket, Prefix: "/pool-one/", AccessKey: accessKey, SecretKey: secretKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Put(ctx, "blocks/a", []byte("the boulder")); err != nil {
		t.Fatal(err)
	}
	if err := client.Put(ctx, "blocks/b", []byte("rolls")); err != nil {
		t.Fatal(err)
	}
	if got, err := client.Get(ctx, "blocks/a"); err != nil || string(got) != "the boulder" {
		t.Errorf("Get = %q, %v", got, err)
	}
	if size, err := client.Size(ctx, "blocks/b"); err != nil || size != 5 {
		t.Errorf("Size = %d, %v", size, err)
	}
	// What is not there is said to be not there, in the store's own words.
	if _, err := client.Get(ctx, "blocks/absent"); !errors.Is(err, storage.ErrNoObject) {
		t.Errorf("Get of nothing: %v", err)
	}
	if _, err := client.Size(ctx, "blocks/absent"); !errors.Is(err, storage.ErrNoObject) {
		t.Errorf("Size of nothing: %v", err)
	}
	// Another prefix in the same bucket is another store's, and unseen.
	other, err := Open(ctx, Config{Endpoint: endpoint(t), Bucket: bucket, AccessKey: accessKey, SecretKey: secretKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Put(ctx, "elsewhere", []byte("x")); err != nil {
		t.Fatal(err)
	}
	listed := map[string]int64{}
	if err := client.List(ctx, func(name string, size int64) bool { listed[name] = size; return true }); err != nil || len(listed) != 2 || listed["blocks/a"] != 11 || listed["blocks/b"] != 5 {
		t.Errorf("List = %v, %v", listed, err)
	}
	// A listing can be stopped part way.
	seen := 0
	if err := client.List(ctx, func(string, int64) bool { seen++; return false }); err != nil || seen != 1 {
		t.Errorf("a listing stopped at once saw %d, %v", seen, err)
	}
	if err := client.Remove(ctx, "blocks/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Size(ctx, "blocks/a"); !errors.Is(err, storage.ErrNoObject) {
		t.Errorf("Size of what was removed: %v", err)
	}

	// And a whole store over it: a blob put, read back and verified.
	store, err := storage.OpenBucket(client, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	blob := bytes.Repeat([]byte("one must imagine Sisyphus happy. "), 20_000)
	c, err := store.Put(ctx, bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(ctx, c); err != nil {
		t.Errorf("Verify: %v", err)
	}
	if size, err := store.Size(ctx); err != nil || size < uint64(len(blob)) {
		t.Errorf("Size = %d, %v", size, err)
	}
}

func TestABucketThatCannotBeUsedIsSaidSo(t *testing.T) {
	ctx := context.Background()
	at := endpoint(t)
	bucket := newBucket(t)
	soon, done := context.WithTimeout(ctx, 3*time.Second)
	defer done()
	for name, tt := range map[string]struct {
		cfg  Config
		want string
	}{
		"an address that is none":        {Config{Endpoint: "http://", Bucket: bucket}, "is not one"},
		"an address that cannot be read": {Config{Endpoint: "http://a b", Bucket: bucket}, "is not one"},
		"no bucket named":                {Config{Endpoint: at}, "no bucket was named"},
		"a host that is no host":         {Config{Endpoint: "http://-not_a_host-:9", Bucket: bucket}, "object store at"},
		"a bucket that is not there":     {Config{Endpoint: at, Bucket: "no-such-bucket-here", AccessKey: accessKey, SecretKey: secretKey}, "there is no bucket no-such-bucket-here"},
		"nothing at the address":         {Config{Endpoint: "127.0.0.1:1", Bucket: bucket}, "reach bucket"},
	} {
		if _, err := Open(soon, tt.cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
	// A listing that fails part way is a failure, not a short list.
	client, err := Open(ctx, Config{Endpoint: at, Bucket: bucket, AccessKey: accessKey, SecretKey: secretKey})
	if err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := client.List(gone, func(string, int64) bool { return true }); err == nil {
		t.Error("a listing that could not be made was taken for an empty one")
	}
	if err := client.Put(gone, "x", []byte("x")); err == nil {
		t.Error("a put that could not be made was taken for done")
	}
}
