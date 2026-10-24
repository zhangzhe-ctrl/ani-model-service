package storage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func factsArchive(t *testing.T, mutate func(*tar.Header)) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	for _, name := range []string{"config.json", "model.safetensors", "tokenizer.json", "tokenizer_config.json"} {
		content := []byte(`{"model_type":"qwen2"}`)
		h := &tar.Header{Name: name, Mode: 0600, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if mutate != nil {
			mutate(h)
		}
		if err := writer.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := writer.Write(content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestMinIOFinetuningFactsPinsOriginalVersionAndRejectsLatest(t *testing.T) {
	for _, mode := range []string{"valid", "no version", "wrong returned version", "changed bytes"} {
		t.Run(mode, func(t *testing.T) {
			raw := factsArchive(t, nil)
			h := sha256.Sum256(raw)
			digest := hex.EncodeToString(h[:])
			gets := 0
			heads := 0
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/models/tenant/model.tar" {
					t.Errorf("unexpected object %s", r.URL)
					w.WriteHeader(404)
					return
				}
				version := "fixed-v1"
				if mode == "no version" {
					version = "null"
				}
				if r.Method == http.MethodGet && mode == "wrong returned version" {
					version = "latest-v2"
				}
				w.Header().Set("x-amz-version-id", version)
				w.Header().Set("ETag", `"etag-is-not-content-sha256"`)
				w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
				w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
				switch r.Method {
				case http.MethodHead:
					heads++
				case http.MethodGet:
					gets++
					if r.URL.Query().Get("versionId") != "fixed-v1" {
						t.Errorf("GET did not pin original version: %s", r.URL)
					}
					body := append([]byte{}, raw...)
					if mode == "changed bytes" {
						body[len(body)-1] = 1
					}
					_, _ = w.Write(body)
				default:
					t.Errorf("facts attempted write %s", r.Method)
					w.WriteHeader(405)
				}
			}))
			defer endpoint.Close()
			client, err := minio.New(strings.TrimPrefix(endpoint.URL, "http://"), &minio.Options{Region: "us-east-1", Creds: credentials.NewStaticV4("access", "secret", ""), Secure: false})
			if err != nil {
				t.Fatal(err)
			}
			adapter := &MinIOAdapter{client: client, bucket: "models"}
			got, err := adapter.InspectFinetuningArtifact(context.Background(), "tenant", "model.tar", int64(len(raw)), digest)
			if mode == "valid" {
				if err != nil || got.VersionID != "fixed-v1" || got.Bucket != "models" || got.Key != "tenant/model.tar" || gets != 1 || heads != 1 {
					t.Fatalf("got=%+v err=%v GET=%d HEAD=%d", got, err, gets, heads)
				}
			} else if err == nil {
				t.Fatalf("%s accepted", mode)
			}
			if mode == "no version" && gets != 0 {
				t.Fatal("read mutable latest object")
			}
		})
	}
}
func TestFinetuningInventoryHashesActualArchiveAndMembers(t *testing.T) {
	raw := factsArchive(t, nil)
	hash := sha256.Sum256(raw)
	got, err := inspectFinetuningArchive(context.Background(), bytes.NewReader(raw), int64(len(raw)), hex.EncodeToString(hash[:]))
	if err != nil || len(got.Files) != 4 || got.Files[0].Path != "config.json" || got.SizeBytes != int64(len(raw)) || !bytes.Equal(got.ConfigJSON, []byte(`{"model_type":"qwen2"}`)) {
		t.Fatalf("inventory=%+v err=%v", got, err)
	}
	for _, file := range got.Files {
		h := sha256.Sum256([]byte(`{"model_type":"qwen2"}`))
		if file.SHA256 != hex.EncodeToString(h[:]) {
			t.Fatal("invented member hash")
		}
	}
}
func TestFinetuningInventoryRejectsUnsafeOrChangedBytes(t *testing.T) {
	for _, test := range []struct {
		name        string
		mutation    func(*tar.Header)
		appendBytes []byte
		wrongHash   bool
		sizeDelta   int64
	}{
		{name: "parent", mutation: func(h *tar.Header) { h.Name = "../" + h.Name }},
		{name: "link", mutation: func(h *tar.Header) { h.Typeflag = tar.TypeSymlink; h.Linkname = "elsewhere"; h.Size = 0 }},
		{name: "duplicate", mutation: func(h *tar.Header) { h.Name = "config.json" }},
		{name: "trailing data", appendBytes: []byte("hidden")},
		{name: "digest", wrongHash: true}, {name: "length", sizeDelta: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := append(factsArchive(t, test.mutation), test.appendBytes...)
			h := sha256.Sum256(raw)
			digest := hex.EncodeToString(h[:])
			if test.wrongHash {
				digest = string(bytes.Repeat([]byte("a"), 64))
			}
			if _, err := inspectFinetuningArchive(context.Background(), bytes.NewReader(raw), int64(len(raw))+test.sizeDelta, digest); err == nil {
				t.Fatal("unsafe bytes accepted")
			}
		})
	}
}
