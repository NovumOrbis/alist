package quark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/op"
	streamPkg "github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/pkg/cookie"
	"github.com/go-resty/resty/v2"
)

func uploadPreTestStream() *streamPkg.FileStream {
	return &streamPkg.FileStream{
		Obj: &model.Object{
			Name: "pre-hardening.bin",
			Size: 1,
		},
		Reader:   strings.NewReader("x"),
		Mimetype: "application/octet-stream",
	}
}

func validUploadPreTestResponse() UpPreResp {
	var pre UpPreResp
	pre.Status = http.StatusOK
	pre.Code = 0
	pre.Data.TaskId = "task"
	pre.Data.UploadId = "upload"
	pre.Data.ObjKey = "obj"
	pre.Data.UploadUrl = "http://oss.test"
	pre.Data.Fid = "fid"
	pre.Data.Bucket = "bucket"
	pre.Data.AuthInfo = "auth"
	pre.Metadata.PartSize = 1
	return pre
}

func waitUploadPreTest[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

type uploadPreRollbackDriver struct {
	*QuarkOrUC
	originalExists bool
	tempExists     bool
	renameCalls    []string
}

func (d *uploadPreRollbackDriver) Get(_ context.Context, path string) (model.Obj, error) {
	clean := strings.TrimPrefix(path, "/")
	switch clean {
	case "":
		return &model.Object{ID: "root", Name: "root", IsFolder: true}, nil
	case "pre-hardening.bin":
		if d.originalExists {
			return &model.Object{ID: "old-fid", Name: "pre-hardening.bin", Size: 10}, nil
		}
	case "pre-hardening.bin.alist_to_delete":
		if d.tempExists {
			return &model.Object{ID: "old-fid", Name: "pre-hardening.bin.alist_to_delete", Size: 10}, nil
		}
	}
	return nil, errs.ObjectNotFound
}

func (d *uploadPreRollbackDriver) Rename(_ context.Context, srcObj model.Obj, newName string) error {
	d.renameCalls = append(d.renameCalls, srcObj.GetName()+"->"+newName)
	switch {
	case srcObj.GetName() == "pre-hardening.bin" &&
		newName == "pre-hardening.bin.alist_to_delete" &&
		d.originalExists && !d.tempExists:
		d.originalExists = false
		d.tempExists = true
		return nil
	case srcObj.GetName() == "pre-hardening.bin.alist_to_delete" &&
		newName == "pre-hardening.bin" &&
		!d.originalExists && d.tempExists:
		d.tempExists = false
		d.originalExists = true
		return nil
	default:
		return errors.New("unexpected rollback rename")
	}
}

func TestUploadPreHardeningPreHTTP500EmptyJSONFailsClosed(t *testing.T) {
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
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil || !strings.Contains(err.Error(), "stage pre") || !strings.Contains(err.Error(), "HTTP status 500") {
		t.Fatalf("err=%v, want staged HTTP 500 failure", err)
	}
	if calls != 1 {
		t.Fatalf("pre calls=%d, want 1", calls)
	}
}

func TestUploadPreHardeningPreHTTP500HTMLFailsClosed(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/clouddrive/file/upload/pre" {
			http.NotFound(w, r)
			return
		}
		calls++
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<html>gateway failure</html>`)
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil || !strings.Contains(err.Error(), "stage pre") || !strings.Contains(err.Error(), "HTTP status 500") {
		t.Fatalf("err=%v, want staged HTTP 500 failure", err)
	}
	if calls != 1 {
		t.Fatalf("pre calls=%d, want 1", calls)
	}
}

func TestUploadPreHardeningPreMalformedJSONFailsClosed(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{")
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil || !strings.Contains(err.Error(), "stage pre") {
		t.Fatalf("err=%v, want malformed JSON PRE failure", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want exactly one PRE", calls)
	}
}

func TestUploadPreHardeningPreHTTP200ProviderRejectionFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/clouddrive/file/upload/pre" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, Resp{Status: 500, Code: 500, Message: "inner error, requestId pre-hardening"})
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil || !strings.Contains(err.Error(), "stage pre") || !strings.Contains(err.Error(), "inner error, requestId pre-hardening") {
		t.Fatalf("err=%v, want staged provider rejection", err)
	}
}

func TestUploadPreHardeningPutStopsBeforeHashOnInvalidPre(t *testing.T) {
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
		if r.URL.Path != "/1/clouddrive/file/upload/pre" {
			t.Errorf("unexpected request after invalid PRE: %s", r.URL.Path)
			return
		}
		pre := validUploadPreTestResponse()
		pre.Metadata.PartSize = 0
		writeJSON(w, http.StatusOK, pre)
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	dst := &model.Object{ID: "parent", Name: "parent", IsFolder: true}
	err := d.Put(context.Background(), dst, uploadPreTestStream(), func(float64) {})
	if err == nil || !strings.Contains(err.Error(), "stage pre") || !strings.Contains(err.Error(), "part_size") {
		t.Fatalf("err=%v, want structural PRE failure before hash", err)
	}

	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) != 1 || gotPaths[0] != "/1/clouddrive/file/upload/pre" {
		t.Fatalf("server paths=%v, want only PRE", gotPaths)
	}
}

func TestUploadPreHardeningOpPutRestoresOldObjectAfterInvalidPre(t *testing.T) {
	oldTempDir := conf.Conf.TempDir
	conf.Conf.TempDir = t.TempDir()
	t.Cleanup(func() {
		conf.Conf.TempDir = oldTempDir
	})

	preCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1/clouddrive/file/upload/pre" {
			t.Errorf("unexpected request after invalid PRE: %s", r.URL.Path)
			return
		}
		preCalls++
		pre := validUploadPreTestResponse()
		pre.Metadata.PartSize = 0
		writeJSON(w, http.StatusOK, pre)
	}))
	defer srv.Close()

	baseDriver := newTestDriver(srv.URL)
	baseDriver.config = driver.Config{
		Name:              "Quark",
		NoOverwriteUpload: true,
	}
	baseDriver.Storage.Driver = "Quark"
	storage := &uploadPreRollbackDriver{
		QuarkOrUC:      baseDriver,
		originalExists: true,
	}

	err := op.Put(context.Background(), storage, "/", uploadPreTestStream(), nil)
	if err == nil || !strings.Contains(err.Error(), "stage pre") || !strings.Contains(err.Error(), "part_size") {
		t.Fatalf("err=%v, want structural PRE failure", err)
	}
	if preCalls != 1 {
		t.Fatalf("preCalls=%d, want 1", preCalls)
	}
	if !storage.originalExists || storage.tempExists {
		t.Fatalf("rollback state original=%v temp=%v, want original restored and temp absent",
			storage.originalExists, storage.tempExists)
	}
	wantRenames := []string{
		"pre-hardening.bin->pre-hardening.bin.alist_to_delete",
		"pre-hardening.bin.alist_to_delete->pre-hardening.bin",
	}
	if len(storage.renameCalls) != len(wantRenames) {
		t.Fatalf("renameCalls=%v, want %v", storage.renameCalls, wantRenames)
	}
	for i := range wantRenames {
		if storage.renameCalls[i] != wantRenames[i] {
			t.Fatalf("renameCalls=%v, want %v", storage.renameCalls, wantRenames)
		}
	}
}

func TestUploadPreHardeningPreTransportErrorUsesSingleWireAttempt(t *testing.T) {
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
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil {
		t.Fatal("want transport error")
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("wire attempts=%d, want exactly 1", got)
	}
	if client.RetryCount != 3 {
		t.Fatalf("source RetryCount=%d, want unchanged 3", client.RetryCount)
	}
}

func TestUploadPreHardeningPreCallerCancellationCancelsInFlightRequest(t *testing.T) {
	started := make(chan struct{}, 1)
	attempts := 0
	var mu sync.Mutex
	client := resty.NewWithClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})})

	d := newTestDriver("http://loopback.invalid")
	d.client = client
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := d.upPreReliable(ctx, uploadPreTestStream(), "parent")
		done <- err
	}()

	waitUploadPreTest(t, started, "PRE RoundTrip start")
	cancel()
	err := waitUploadPreTest(t, done, "canceled PRE return")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("wire attempts=%d, want 1", got)
	}
}

func TestUploadPreHardeningPre307IsRejectedWithoutReplay(t *testing.T) {
	var mu sync.Mutex
	methods := make([]string, 0, 2)
	paths := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/1/clouddrive/file/upload/pre" {
			w.Header().Set("Location", "/redirected-pre")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		t.Errorf("redirect replay reached %s", r.URL.Path)
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil || !strings.Contains(err.Error(), "HTTP status 307") {
		t.Fatalf("err=%v, want HTTP 307 failure", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != http.MethodPost || len(paths) != 1 || paths[0] != "/1/clouddrive/file/upload/pre" {
		t.Fatalf("methods=%v paths=%v, want one PRE POST", methods, paths)
	}
}

func TestUploadPreHardeningPre302IsRejectedWithoutMethodChange(t *testing.T) {
	var mu sync.Mutex
	methods := make([]string, 0, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		if r.URL.Path == "/1/clouddrive/file/upload/pre" {
			w.Header().Set("Location", "/redirected-pre")
			w.WriteHeader(http.StatusFound)
			return
		}
		t.Errorf("redirect replay reached %s", r.URL.Path)
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil || !strings.Contains(err.Error(), "HTTP status 302") {
		t.Fatalf("err=%v, want HTTP 302 failure", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 1 || methods[0] != http.MethodPost {
		t.Fatalf("methods=%v, want [POST]", methods)
	}
}

func TestUploadPreHardeningPreCrossOriginRedirectIsNotFollowed(t *testing.T) {
	redirectedCalls := 0
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedCalls++
		writeJSON(w, http.StatusOK, validUploadPreTestResponse())
	}))
	defer redirected.Close()

	originCalls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls++
		w.Header().Set("Location", redirected.URL+"/pre")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	d := newTestDriver(origin.URL)
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil || !strings.Contains(err.Error(), "HTTP status 307") {
		t.Fatalf("err=%v, want HTTP 307 failure", err)
	}
	if originCalls != 1 || redirectedCalls != 0 {
		t.Fatalf("originCalls=%d redirectedCalls=%d, want 1/0", originCalls, redirectedCalls)
	}
}

func TestUploadPreHardeningPreProviderErrorIsReportedForQuarkAndUC(t *testing.T) {
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
					Message: "inner error, requestId pre-hardening-provider",
				})
			}))
			defer srv.Close()

			d := newTestDriver(srv.URL)
			d.config.Name = tc.name
			d.conf = Conf{
				ua:      "pre-hardening-test-ua",
				referer: tc.referer,
				api:     srv.URL + "/1/clouddrive",
				pr:      tc.pr,
			}
			_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
			if err == nil || !strings.Contains(err.Error(), "stage pre") || !strings.Contains(err.Error(), "inner error") {
				t.Fatalf("err=%v, want staged provider error", err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d, want 1", calls)
			}
		})
	}
}

func TestUploadPreHardeningPreCanceledBeforeStartMakesNoRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, http.StatusOK, validUploadPreTestResponse())
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := d.upPreReliable(ctx, uploadPreTestStream(), "parent")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("calls=%d, want 0", calls)
	}
}

func TestUploadPreHardeningValidPreContinuesExistingTask(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, http.StatusOK, validUploadPreTestResponse())
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	pre, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err != nil {
		t.Fatalf("upPreReliable: %v", err)
	}
	if calls != 1 || pre.Data.TaskId != "task" || pre.Data.Fid != "fid" || pre.Metadata.PartSize != 1 {
		t.Fatalf("calls=%d pre=%+v, want one valid PRE", calls, pre)
	}
}

func TestUploadPreHardeningPrePreservesRequestShape(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/1/clouddrive/file/upload/pre" {
			t.Fatalf("method/path=%s %s, want PRE POST", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("pr") != "ucpro" || r.URL.Query().Get("fr") != "pc" {
			t.Fatalf("query=%v, want pr=ucpro fr=pc", r.URL.Query())
		}
		if r.Header.Get("Accept") != "application/json, text/plain, */*" {
			t.Fatalf("Accept=%q", r.Header.Get("Accept"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode PRE body: %v", err)
		}
		if body["file_name"] != "pre-hardening.bin" ||
			body["format_type"] != "application/octet-stream" ||
			body["pdir_fid"] != "parent" ||
			body["dir_name"] != "" ||
			body["size"] != float64(1) ||
			body["ccp_hash_update"] != true {
			t.Fatalf("unexpected PRE body: %#v", body)
		}
		if _, ok := body["same_path_reuse"]; ok {
			t.Fatalf("same_path_reuse must remain disabled: %#v", body)
		}
		if _, ok := body["l_created_at"].(float64); !ok {
			t.Fatalf("l_created_at missing/not numeric: %#v", body["l_created_at"])
		}
		if _, ok := body["l_updated_at"].(float64); !ok {
			t.Fatalf("l_updated_at missing/not numeric: %#v", body["l_updated_at"])
		}
		writeJSON(w, http.StatusOK, validUploadPreTestResponse())
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	if _, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent"); err != nil {
		t.Fatalf("upPreReliable: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

func TestUploadPreHardeningPreRequiresExactHTTP200(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusCreated {
					writeJSON(w, status, validUploadPreTestResponse())
					return
				}
				w.WriteHeader(status)
			}))
			defer srv.Close()

			d := newTestDriver(srv.URL)
			_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
			if err == nil || !strings.Contains(err.Error(), "unexpected HTTP status") {
				t.Fatalf("status=%d err=%v, want HTTP status failure", status, err)
			}
		})
	}
}

func TestUploadPreHardeningPreStructuralFieldsFailClosed(t *testing.T) {
	basePre := validUploadPreTestResponse()
	tests := []struct {
		name   string
		mutate func(*UpPreResp)
		want   string
	}{
		{name: "task_id", mutate: func(pre *UpPreResp) { pre.Data.TaskId = "" }, want: "task_id"},
		{name: "fid", mutate: func(pre *UpPreResp) { pre.Data.Fid = "" }, want: "fid"},
		{name: "upload_id", mutate: func(pre *UpPreResp) { pre.Data.UploadId = "" }, want: "upload_id"},
		{name: "obj_key", mutate: func(pre *UpPreResp) { pre.Data.ObjKey = "" }, want: "obj_key"},
		{name: "bucket", mutate: func(pre *UpPreResp) { pre.Data.Bucket = "" }, want: "bucket"},
		{name: "auth_info", mutate: func(pre *UpPreResp) { pre.Data.AuthInfo = "" }, want: "auth_info"},
		{name: "part_size_zero", mutate: func(pre *UpPreResp) { pre.Metadata.PartSize = 0 }, want: "part_size"},
		{name: "part_size_negative", mutate: func(pre *UpPreResp) { pre.Metadata.PartSize = -1 }, want: "part_size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pre := basePre
			tc.mutate(&pre)
			err := validateQuarkUploadPre(pre)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want validation failure containing %q", err, tc.want)
			}
		})
	}
}

func TestUploadPreHardeningPreUploadURLShapeMatchesBaselineTargetUse(t *testing.T) {
	valid := []string{
		"http://host",
		"http://host:443",
		"abcd://host",
		"HTTP://host",
		"http://127.0.0.1",
	}
	for _, uploadURL := range valid {
		t.Run("valid_"+strings.ReplaceAll(uploadURL, "/", "_"), func(t *testing.T) {
			pre := validUploadPreTestResponse()
			pre.Data.UploadUrl = uploadURL
			if err := validateQuarkUploadPre(pre); err != nil {
				t.Fatalf("upload_url=%q err=%v, want valid baseline-compatible target", uploadURL, err)
			}
		})
	}

	invalid := []string{
		"",
		"http://",
		"https://host",
		"synthetic",
		"ftp://host",
		"http:///host",
		"http://host/path",
		"http://host/",
		"http://host?x=1",
		"http://host#fragment",
		"http://[::1]",
		"http://user@host",
		"http://host\x00",
	}
	for _, uploadURL := range invalid {
		t.Run("invalid_"+strings.ReplaceAll(uploadURL, "/", "_"), func(t *testing.T) {
			pre := validUploadPreTestResponse()
			pre.Data.UploadUrl = uploadURL
			if err := validateQuarkUploadPre(pre); err == nil {
				t.Fatalf("upload_url=%q unexpectedly accepted", uploadURL)
			}
		})
	}
}

type uploadPreErrorReader struct {
	data []byte
	err  error
}

func (r *uploadPreErrorReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *uploadPreErrorReader) Close() error {
	return nil
}

func TestUploadPreHardeningPrePartialBodyReadErrorIsNotRetried(t *testing.T) {
	attempts := 0
	client := base.NewRestyClient()
	client.SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     headers,
			Body: &uploadPreErrorReader{
				data: []byte(`{"status":200,"code":0,"data":{"task_id":"task"`),
				err:  io.ErrUnexpectedEOF,
			},
			Request: req,
		}, nil
	}))

	d := newTestDriver("http://loopback.invalid")
	d.client = client
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil {
		t.Fatal("want partial-body read error")
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want 1", attempts)
	}
}

func TestUploadPreHardeningPreMergesResponseSessionCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "new-session", Path: "/"})
		writeJSON(w, http.StatusOK, validUploadPreTestResponse())
	}))
	defer srv.Close()

	d := newTestDriver(srv.URL)
	d.Cookie = "a=1; __puus=old-session; b=2"
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err != nil {
		t.Fatalf("upPreReliable: %v", err)
	}
	if got := cookie.GetStr(d.Cookie, "__puus"); got != "new-session" {
		t.Fatalf("__puus=%q, want new-session", got)
	}
	if !strings.Contains(d.Cookie, "a=1") || !strings.Contains(d.Cookie, "b=2") {
		t.Fatalf("cookie merge dropped unrelated cookies: %q", d.Cookie)
	}
}

func TestUploadPreHardeningPreClientPreservesTransportJarTimeoutAndHeaders(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	transport := &http.Transport{}
	sourceHTTP := &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   1234 * time.Millisecond,
	}
	source := resty.NewWithClient(sourceHTTP).
		SetHeader("User-Agent", "pre-hardening-test-agent").
		SetRetryCount(3)

	d := newTestDriver("http://loopback.invalid")
	d.client = source
	preClient := d.preRequestClient()

	if preClient.RetryCount != 0 {
		t.Fatalf("PRE RetryCount=%d, want 0", preClient.RetryCount)
	}
	if preClient.GetClient().Transport != sourceHTTP.Transport {
		t.Fatal("PRE transport does not reuse selected source transport")
	}
	if preClient.GetClient().Jar != sourceHTTP.Jar {
		t.Fatal("PRE cookie jar does not reuse selected source jar")
	}
	if preClient.GetClient().Timeout != sourceHTTP.Timeout {
		t.Fatalf("PRE timeout=%v, want %v", preClient.GetClient().Timeout, sourceHTTP.Timeout)
	}
	if preClient.GetClient().CheckRedirect == nil {
		t.Fatal("PRE redirect policy is not disabled")
	}
	if got := preClient.Header.Get("User-Agent"); got != "pre-hardening-test-agent" {
		t.Fatalf("PRE User-Agent=%q, want copied source header", got)
	}
	if source.RetryCount != 3 || source.GetClient().CheckRedirect != nil {
		t.Fatal("building PRE client mutated selected source client")
	}
}

func TestUploadPreHardeningPreDoesNotMutateSelectedRestyClient(t *testing.T) {
	var redirected int
	redirectedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected++
		writeJSON(w, http.StatusOK, validUploadPreTestResponse())
	}))
	defer redirectedServer.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", redirectedServer.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := base.NewRestyClient()
	if client.GetClient().CheckRedirect != nil {
		t.Fatal("test requires the source client to start with the default redirect policy")
	}
	d := newTestDriver(origin.URL)
	d.client = client
	_, err := d.upPreReliable(context.Background(), uploadPreTestStream(), "parent")
	if err == nil || !strings.Contains(err.Error(), "HTTP status 307") {
		t.Fatalf("err=%v, want redirect rejection", err)
	}

	if client.RetryCount != 3 {
		t.Fatalf("RetryCount=%d, want source client unchanged at 3", client.RetryCount)
	}
	if client.GetClient().CheckRedirect != nil {
		t.Fatal("source http.Client CheckRedirect was mutated")
	}
	if redirected != 0 {
		t.Fatalf("redirected calls=%d, want 0", redirected)
	}
}
