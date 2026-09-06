package quark

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWaitUploadedObjectVisibleRequiresMatchingKnownFid(t *testing.T) {
	listCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/1/clouddrive/file/sort" {
			http.NotFound(w, r)
			return
		}
		listCalls++
		writeJSON(w, http.StatusOK, map[string]any{
			"status": 200,
			"code":   0,
			"data": map[string]any{"list": []map[string]any{{
				"fid": "stale-old-fid", "file_name": "857.bucket.2", "file": true, "size": 123,
			}}},
			"metadata": map[string]any{"_size": 100, "_page": 1, "_count": 1, "_total": 1},
		})
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	var pre UpPreResp
	pre.Data.Fid = "new-upload-fid"

	visible, err := d.waitUploadedObjectVisible(context.Background(), pre, "parent", "857.bucket.2", 123)
	if err != nil {
		t.Fatalf("waitUploadedObjectVisible: %v", err)
	}
	if visible {
		t.Fatal("stale same-name same-size object with a different fid must not verify the current upload")
	}
	if listCalls != uploadFinishVisibilityAttempts {
		t.Fatalf("listCalls=%d, want %d", listCalls, uploadFinishVisibilityAttempts)
	}
}
