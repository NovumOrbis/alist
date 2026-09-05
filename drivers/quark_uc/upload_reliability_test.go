package quark

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsRetryableQuarkUploadControlError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "lock timeout", err: errors.New("complete_upload_lock_timeout"), want: true},
		{name: "lock timeout with detail", err: errors.New("complete_upload_lock_timeout, requestId abc"), want: true},
		{name: "inner error request id", err: errors.New("inner error, requestId 396ius-abc"), want: true},
		{name: "generic inner error", err: errors.New("inner error"), want: false},
		{name: "oss formatted inner error must not replay", err: errors.New("up status: 500, error: inner error, requestId abc"), want: false},
		{name: "same-name conflict", err: errors.New(mkdirConflictMessage), want: false},
		{name: "transport timeout", err: errors.New("context deadline exceeded"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableQuarkUploadControlError(tt.err); got != tt.want {
				t.Fatalf("isRetryableQuarkUploadControlError(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryQuarkUploadControlRetriesTransientThenSucceeds(t *testing.T) {
	calls := 0
	got, err := retryQuarkUploadControl(context.Background(), "hash", func() (int, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("inner error, requestId test-1")
		}
		return 42, nil
	})
	if err != nil {
		t.Fatalf("retryQuarkUploadControl: %v", err)
	}
	if got != 42 || calls != 2 {
		t.Fatalf("got=%d calls=%d, want 42/2", got, calls)
	}
}

func TestRetryQuarkUploadControlDoesNotRetryNonTransient(t *testing.T) {
	calls := 0
	_, err := retryQuarkUploadControl(context.Background(), "hash", func() (int, error) {
		calls++
		return 0, errors.New("permission denied")
	})
	if err == nil {
		t.Fatal("want error")
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if !strings.Contains(err.Error(), "stage hash") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("unexpected staged error: %v", err)
	}
}

func TestRetryQuarkUploadControlHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := retryQuarkUploadControl(ctx, "hash", func() (int, error) {
		calls++
		return 0, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("calls=%d, want 0", calls)
	}
}

func TestUpHashReliableRetriesProviderTransient(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/1/clouddrive/file/update/hash" {
			http.NotFound(w, r)
			return
		}
		calls++
		if calls == 1 {
			writeJSON(w, http.StatusInternalServerError, Resp{Status: 500, Code: 500, Message: "inner error, requestId hash-1"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status": 200,
			"code":   0,
			"data":   map[string]any{"finish": false},
		})
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	finish, err := d.upHashReliable(context.Background(), "md5", "sha1", "task-1")
	if err != nil {
		t.Fatalf("upHashReliable: %v", err)
	}
	if finish {
		t.Fatal("finish=true, want false")
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

func TestUpFinishReliableTreatsVisibleObjectAsSuccess(t *testing.T) {
	finishCalls := 0
	listCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/1/clouddrive/file/upload/finish":
			finishCalls++
			writeJSON(w, http.StatusInternalServerError, Resp{Status: 500, Code: 500, Message: "complete_upload_lock_timeout"})
		case r.Method == http.MethodGet && r.URL.Path == "/1/clouddrive/file/sort":
			listCalls++
			writeJSON(w, http.StatusOK, map[string]any{
				"status": 200,
				"code":   0,
				"data": map[string]any{"list": []map[string]any{{
					"fid": "fid-857", "file_name": "857.bucket.2", "file": true, "size": 123,
				}}},
				"metadata": map[string]any{"_size": 100, "_page": 1, "_count": 1, "_total": 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	var pre UpPreResp
	pre.Data.TaskId = "task-857"
	pre.Data.ObjKey = "obj-857"
	pre.Data.Fid = "fid-857"
	if err := d.upFinishReliable(context.Background(), pre, "parent", "857.bucket.2", 123); err != nil {
		t.Fatalf("upFinishReliable: %v", err)
	}
	if finishCalls != 1 || listCalls != 1 {
		t.Fatalf("finishCalls=%d listCalls=%d, want 1/1", finishCalls, listCalls)
	}
}

func TestUpFinishReliableRetriesWhenTransientObjectNotVisible(t *testing.T) {
	finishCalls := 0
	listCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/1/clouddrive/file/upload/finish":
			finishCalls++
			if finishCalls == 1 {
				writeJSON(w, http.StatusInternalServerError, Resp{Status: 500, Code: 500, Message: "inner error, requestId finish-1"})
				return
			}
			writeJSON(w, http.StatusOK, Resp{Status: 200, Code: 0, Message: "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/1/clouddrive/file/sort":
			listCalls++
			writeJSON(w, http.StatusOK, map[string]any{
				"status":   200,
				"code":     0,
				"data":     map[string]any{"list": []any{}},
				"metadata": map[string]any{"_size": 100, "_page": 1, "_count": 0, "_total": 0},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	var pre UpPreResp
	pre.Data.TaskId = "task-1"
	pre.Data.ObjKey = "obj-1"
	if err := d.upFinishReliable(context.Background(), pre, "parent", "x.bucket.2", 456); err != nil {
		t.Fatalf("upFinishReliable: %v", err)
	}
	if finishCalls != 2 {
		t.Fatalf("finishCalls=%d, want 2", finishCalls)
	}
	if listCalls != uploadFinishVisibilityAttempts {
		t.Fatalf("listCalls=%d, want %d", listCalls, uploadFinishVisibilityAttempts)
	}
}

func TestUpFinishReliablePreservesNonTransientError(t *testing.T) {
	finishCalls := 0
	listCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1/clouddrive/file/upload/finish":
			finishCalls++
			writeJSON(w, http.StatusForbidden, Resp{Status: 403, Code: 403, Message: "permission denied"})
		case "/1/clouddrive/file/sort":
			listCalls++
			writeJSON(w, http.StatusOK, Resp{Status: 200, Code: 0, Message: "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	var pre UpPreResp
	pre.Data.TaskId = "task-1"
	pre.Data.ObjKey = "obj-1"
	err := d.upFinishReliable(context.Background(), pre, "parent", "x.bucket.2", 1)
	if err == nil {
		t.Fatal("want error")
	}
	if finishCalls != 1 || listCalls != 0 {
		t.Fatalf("finishCalls=%d listCalls=%d, want 1/0", finishCalls, listCalls)
	}
	if got := err.Error(); !strings.Contains(got, "stage finish") || !strings.Contains(got, "permission denied") {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestWaitUploadedObjectVisibleHonorsCanceledContext(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	visible, err := d.waitUploadedObjectVisible(ctx, UpPreResp{}, "parent", "x", 1)
	if visible || !errors.Is(err, context.Canceled) {
		t.Fatalf("visible=%v err=%v, want false/context.Canceled", visible, err)
	}
	if requests != 0 {
		t.Fatalf("requests=%d, want 0", requests)
	}
}

func TestWrapQuarkUploadStagePreservesCause(t *testing.T) {
	cause := errors.New("boom")
	err := wrapQuarkUploadStage("finish", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("wrapped error does not preserve cause: %v", err)
	}
	if got, want := err.Error(), fmt.Sprintf("quark upload stage finish: %v", cause); got != want {
		t.Fatalf("error=%q, want %q", got, want)
	}
}
