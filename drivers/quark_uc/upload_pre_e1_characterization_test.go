package quark

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/model"
	streamPkg "github.com/alist-org/alist/v3/internal/stream"
	"github.com/go-resty/resty/v2"
)

func e1UploadStream() *streamPkg.FileStream {
	return &streamPkg.FileStream{
		Obj: &model.Object{
			Name: "e1.bin",
			Size: 1,
		},
		Reader:   strings.NewReader("x"),
		Mimetype: "application/octet-stream",
	}
}

func e1ValidPreBody() string {
	return `{"status":200,"code":0,"data":{"task_id":"task","upload_id":"upload","obj_key":"obj","upload_url":"http://oss.test","fid":"fid","bucket":"bucket","auth_info":"auth"},"metadata":{"part_size":1}}`
}

func TestE1BaselinePreHTTP500EmptyJSONCanFalseSucceed(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/1/clouddrive/file/upload/pre" {
			http.NotFound(w, r)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	pre, err := d.upPreReliable(context.Background(), e1UploadStream(), "parent")
	if err != nil {
		t.Fatalf("baseline unexpectedly rejected empty 500 response: %v", err)
	}
	if calls != 1 {
		t.Fatalf("pre calls=%d, want 1", calls)
	}
	if pre.Data.TaskId != "" || pre.Metadata.PartSize != 0 {
		t.Fatalf("pre=%+v, want zero-valued PRE", pre)
	}
}

func TestE1BaselinePreHTTP500HTMLCanFalseSucceed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/clouddrive/file/upload/pre" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<html>gateway failure</html>`)
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	pre, err := d.upPreReliable(context.Background(), e1UploadStream(), "parent")
	if err != nil {
		t.Fatalf("baseline unexpectedly rejected HTML 500 response: %v", err)
	}
	if pre.Data.TaskId != "" || pre.Metadata.PartSize != 0 {
		t.Fatalf("pre=%+v, want zero-valued PRE", pre)
	}
}

func TestE1BaselinePreHTTP200ProviderRejectionCanFalseSucceed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/clouddrive/file/upload/pre" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, Resp{Status: 500, Code: 500, Message: "inner error, requestId e1"})
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	pre, err := d.upPreReliable(context.Background(), e1UploadStream(), "parent")
	if err != nil {
		t.Fatalf("baseline unexpectedly rejected HTTP-200 provider rejection: %v", err)
	}
	if pre.Status != 500 || pre.Code != 500 {
		t.Fatalf("status/code=%d/%d, want decoded provider rejection 500/500", pre.Status, pre.Code)
	}
}

func TestE1BaselinePutCanPanicAfterZeroPre(t *testing.T) {
	oldTempDir := conf.Conf.TempDir
	conf.Conf.TempDir = t.TempDir()
	t.Cleanup(func() {
		conf.Conf.TempDir = oldTempDir
	})

	var mu sync.Mutex
	paths := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/1/clouddrive/file/upload/pre", "/1/clouddrive/file/update/hash":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	dst := &model.Object{ID: "parent", Name: "parent", IsFolder: true}
	var putErr error

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("Put returned without expected divide-by-zero panic: %v", putErr)
		}
		runtimeErr, ok := r.(runtime.Error)
		if !ok || !strings.Contains(runtimeErr.Error(), "integer divide by zero") {
			t.Fatalf("panic=%v, want runtime integer divide by zero", r)
		}
		mu.Lock()
		gotPaths := append([]string(nil), paths...)
		mu.Unlock()
		if len(gotPaths) != 2 ||
			gotPaths[0] != "/1/clouddrive/file/upload/pre" ||
			gotPaths[1] != "/1/clouddrive/file/update/hash" {
			t.Fatalf("server paths=%v, want [pre hash]", gotPaths)
		}
	}()
	putErr = d.Put(context.Background(), dst, e1UploadStream(), func(float64) {})
}

func TestE1BaselinePreTransportErrorUsesOnePlusThreeRestyAttempts(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	client := base.NewRestyClient()
	client.SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return nil, io.ErrUnexpectedEOF
	}))

	d := newTestDriver("http://loopback.invalid")
	d.client = client
	_, err := d.upPreReliable(context.Background(), e1UploadStream(), "parent")
	if err == nil {
		t.Fatal("want transport error")
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 4 {
		t.Fatalf("wire attempts=%d, want 4 (initial + 3 Resty retries)", got)
	}
}

func TestE1BaselinePreCallerCancellationDoesNotCancelInFlightRequest(t *testing.T) {
	started := make(chan struct{}, 1)
	inspectContext := make(chan struct{})
	requestContextErr := make(chan error, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	t.Cleanup(releaseRequest)

	client := resty.NewWithClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-inspectContext
		requestContextErr <- req.Context().Err()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release:
			headers := make(http.Header)
			headers.Set("Content-Type", "application/json")
			return ossResponse(req, http.StatusOK, e1ValidPreBody(), headers), nil
		}
	})})

	d := newTestDriver("http://loopback.invalid")
	d.client = client
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := d.upPreReliable(ctx, e1UploadStream(), "parent")
		done <- err
	}()

	<-started
	cancel()
	close(inspectContext)

	if err := <-requestContextErr; err != nil {
		t.Fatalf("request context observed caller cancellation; baseline unexpectedly propagated caller ctx: %v", err)
	}

	releaseRequest()
	if err := <-done; err != nil {
		t.Fatalf("request after release: %v", err)
	}
}

func TestE1BaselinePre307ReplaysPOSTBody(t *testing.T) {
	var mu sync.Mutex
	methods := make([]string, 0, 2)
	bodies := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		methods = append(methods, r.Method)
		bodies = append(bodies, string(body))
		mu.Unlock()
		switch r.URL.Path {
		case "/1/clouddrive/file/upload/pre":
			w.Header().Set("Location", "/redirected-pre")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/redirected-pre":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, e1ValidPreBody())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	pre, err := d.upPreReliable(context.Background(), e1UploadStream(), "parent")
	if err != nil {
		t.Fatalf("upPreReliable: %v", err)
	}
	if pre.Data.TaskId != "task" {
		t.Fatalf("task_id=%q, want task", pre.Data.TaskId)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 || methods[0] != http.MethodPost || methods[1] != http.MethodPost {
		t.Fatalf("methods=%v, want [POST POST]", methods)
	}
	if bodies[0] == "" || bodies[0] != bodies[1] {
		t.Fatalf("redirect body mismatch: first=%q second=%q", bodies[0], bodies[1])
	}
}

func TestE1BaselinePre302ConvertsPOSTToGET(t *testing.T) {
	var mu sync.Mutex
	methods := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		switch r.URL.Path {
		case "/1/clouddrive/file/upload/pre":
			w.Header().Set("Location", "/redirected-pre")
			w.WriteHeader(http.StatusFound)
		case "/redirected-pre":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, e1ValidPreBody())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	_, err := d.upPreReliable(context.Background(), e1UploadStream(), "parent")
	if err != nil {
		t.Fatalf("upPreReliable: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 2 || methods[0] != http.MethodPost || methods[1] != http.MethodGet {
		t.Fatalf("methods=%v, want [POST GET]", methods)
	}
}

func TestE1BaselinePreProviderErrorIsReportedForQuarkAndUC(t *testing.T) {
	cases := []struct {
		name    string
		referer string
		pr      string
	}{
		{name: "Quark", referer: "https://pan.quark.cn", pr: "ucpro"},
		{name: "UC", referer: "https://drive.uc.cn", pr: "UCBrowser"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/1/clouddrive/file/upload/pre" {
					t.Errorf("path=%q, want PRE path", r.URL.Path)
				}
				if got := r.URL.Query().Get("pr"); got != tc.pr {
					t.Errorf("pr=%q, want %q", got, tc.pr)
				}
				if got := r.Header.Get("Referer"); got != tc.referer {
					t.Errorf("Referer=%q, want %q", got, tc.referer)
				}
				writeJSON(w, http.StatusInternalServerError, Resp{
					Status:  500,
					Code:    500,
					Message: "inner error, requestId e1-provider",
				})
			}))
			defer srv.Close()

			d := newTestDriver(srv.URL)
			d.config.Name = tc.name
			d.conf = Conf{
				ua:      "e1-test-ua",
				referer: tc.referer,
				api:     srv.URL + "/1/clouddrive",
				pr:      tc.pr,
			}
			_, err := d.upPreReliable(context.Background(), e1UploadStream(), "parent")
			if err == nil || !strings.Contains(err.Error(), "stage pre") || !strings.Contains(err.Error(), "inner error") {
				t.Fatalf("err=%v, want staged provider error", err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d, want 1", calls)
			}
		})
	}
}

func TestE1BaselinePreCanceledBeforeStartMakesNoRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, http.StatusOK, map[string]any{"status": 200, "code": 0})
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.upPreReliable(ctx, e1UploadStream(), "parent")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("calls=%d, want 0", calls)
	}
}
