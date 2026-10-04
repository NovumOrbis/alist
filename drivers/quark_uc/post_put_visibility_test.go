package quark

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	streamPkg "github.com/alist-org/alist/v3/internal/stream"
)

var _ driver.PutResult = (*QuarkOrUC)(nil)

func TestPutReturnsPreallocatedFIDForImmediateFollowupOperations(t *testing.T) {
	const (
		parentID = "parent-1"
		fid      = "fid-lock-1"
		name     = "lock_keep_alive.@writer_version_0.test"
	)
	payload := []byte("lock")

	preCalls := 0
	hashCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/1/clouddrive/file/upload/pre":
			preCalls++
			writeJSON(w, http.StatusOK, map[string]any{
				"status": 200,
				"code":   0,
				"data": map[string]any{
					"task_id": "task-1",
					"fid":     fid,
				},
				"metadata": map[string]any{
					"part_size": 4 * 1024 * 1024,
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/1/clouddrive/file/update/hash":
			hashCalls++
			writeJSON(w, http.StatusOK, map[string]any{
				"status": 200,
				"code":   0,
				"data": map[string]any{
					"finish": true,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	parent := &model.Object{ID: parentID, Name: "parent", IsFolder: true}
	fs := &streamPkg.FileStream{
		Obj: &model.Object{
			Name: name,
			Size: int64(len(payload)),
		},
		Reader:   bytes.NewReader(payload),
		Mimetype: "application/octet-stream",
	}
	defer fs.Close()

	obj, err := d.Put(context.Background(), parent, fs, func(float64) {})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if obj == nil {
		t.Fatal("Put returned nil object; op.Put cannot seed the parent cache")
	}
	if obj.GetID() != fid {
		t.Fatalf("Put object fid=%q, want %q", obj.GetID(), fid)
	}
	if obj.GetName() != name {
		t.Fatalf("Put object name=%q, want %q", obj.GetName(), name)
	}
	if obj.GetSize() != int64(len(payload)) {
		t.Fatalf("Put object size=%d, want %d", obj.GetSize(), len(payload))
	}
	if obj.IsDir() {
		t.Fatal("Put object is a directory, want file")
	}
	if preCalls != 1 || hashCalls != 1 {
		t.Fatalf("preCalls=%d hashCalls=%d, want 1/1", preCalls, hashCalls)
	}
}

func TestUploadedFileFromPreDoesNotInventMissingFID(t *testing.T) {
	fs := &streamPkg.FileStream{
		Obj: &model.Object{Name: "lock", Size: 1},
		Reader: bytes.NewReader([]byte{1}),
	}
	if obj := uploadedFileFromPre(UpPreResp{}, fs); obj != nil {
		t.Fatalf("uploadedFileFromPre without fid = %#v, want nil", obj)
	}
}
