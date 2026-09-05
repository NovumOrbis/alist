package quark

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

const (
	mkdirConflictMessage          = "file is doloading[同名冲突]"
	mkdirVisibilityAttempts       = 6
	mkdirVisibilityInitialBackoff = 200 * time.Millisecond
	mkdirVisibilityMaxBackoff     = 800 * time.Millisecond
)

type QuarkOrUC struct {
	model.Storage
	Addition
	config driver.Config
	conf   Conf
}

func (d *QuarkOrUC) Config() driver.Config {
	return d.config
}

func (d *QuarkOrUC) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *QuarkOrUC) Init(ctx context.Context) error {
	_, err := d.request("/config", http.MethodGet, nil, nil)
	return err
}

func (d *QuarkOrUC) Drop(ctx context.Context) error {
	return nil
}

func (d *QuarkOrUC) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.GetFiles(dir.GetID())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *QuarkOrUC) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	data := base.Json{
		"fids": []string{file.GetID()},
	}
	var resp DownResp
	ua := d.conf.ua
	_, err := d.request("/file/download", http.MethodPost, func(req *resty.Request) {
		req.SetHeader("User-Agent", ua).
			SetBody(data)
	}, &resp)
	if err != nil {
		return nil, err
	}

	return &model.Link{
		URL: resp.Data[0].DownloadUrl,
		Header: http.Header{
			"Cookie":     []string{d.Cookie},
			"Referer":    []string{d.conf.referer},
			"User-Agent": []string{ua},
		},
		Concurrency: 2,
		PartSize:    10 * utils.MB,
	}, nil
}

func (d *QuarkOrUC) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	data := base.Json{
		"dir_init_lock": false,
		"dir_path":      "",
		"file_name":     dirName,
		"pdir_fid":      parentDir.GetID(),
	}
	_, err := d.request("/file", http.MethodPost, func(req *resty.Request) {
		req.SetContext(ctx).SetBody(data)
	}, nil)
	if err != nil && err.Error() != mkdirConflictMessage {
		return nil, err
	}

	// Quark may acknowledge a mkdir before /file/sort reflects the new
	// directory. Confirm visibility and return the verified object so op.MakeDir
	// can update an existing parent cache directly instead of immediately
	// re-listing the eventually-consistent backend.
	obj, listed, visibilityErr := d.waitForDirVisible(ctx, parentDir.GetID(), dirName)
	if visibilityErr == nil {
		return obj, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		// Preserve the existing Quark conflict error when the directory cannot
		// be confirmed. A same-name conflict is only treated as success after
		// the matching directory is visible in the parent listing.
		return nil, err
	}
	if !listed {
		// The create request succeeded, but every visibility probe failed before
		// producing a usable listing. Preserve the successful mkdir result rather
		// than turning an unrelated listing outage into a false create failure.
		// Returning a nil object with nil error makes op.MakeDir clear the parent
		// cache, which is safer than leaving a potentially stale listing in place.
		return nil, nil
	}
	return nil, visibilityErr
}

func (d *QuarkOrUC) waitForDirVisible(ctx context.Context, parentID, dirName string) (model.Obj, bool, error) {
	backoff := mkdirVisibilityInitialBackoff
	var lastErr error
	listed := false
	for attempt := 0; attempt < mkdirVisibilityAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, listed, err
		}

		obj, err := d.findDir(ctx, parentID, dirName)
		if err == nil {
			listed = true
			if obj != nil {
				return obj, true, nil
			}
		} else {
			lastErr = err
		}
		if attempt == mkdirVisibilityAttempts-1 {
			break
		}

		select {
		case <-ctx.Done():
			return nil, listed, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < mkdirVisibilityMaxBackoff {
			backoff *= 2
			if backoff > mkdirVisibilityMaxBackoff {
				backoff = mkdirVisibilityMaxBackoff
			}
		}
	}

	if lastErr != nil {
		return nil, listed, fmt.Errorf("failed to confirm created directory %q: %w", dirName, lastErr)
	}
	return nil, listed, fmt.Errorf("created directory %q did not become visible after %d attempts", dirName, mkdirVisibilityAttempts)
}

func (d *QuarkOrUC) findDir(ctx context.Context, parentID, dirName string) (model.Obj, error) {
	const size = 100
	query := map[string]string{
		"pdir_fid":     parentID,
		"_size":        strconv.Itoa(size),
		"_fetch_total": "1",
	}
	for page := 1; ; page++ {
		query["_page"] = strconv.Itoa(page)
		var resp SortResp
		_, err := d.request("/file/sort", http.MethodGet, func(req *resty.Request) {
			req.SetContext(ctx).SetQueryParams(query)
		}, &resp)
		if err != nil {
			return nil, err
		}
		for _, file := range resp.Data.List {
			file.FileName = html.UnescapeString(file.FileName)
			if !file.File && file.FileName == dirName {
				return fileToObj(file), nil
			}
		}
		if page*size >= resp.Metadata.Total {
			return nil, nil
		}
	}
}

func (d *QuarkOrUC) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	data := base.Json{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{srcObj.GetID()},
		"to_pdir_fid":  dstDir.GetID(),
	}
	_, err := d.request("/file/move", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *QuarkOrUC) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	data := base.Json{
		"fid":       srcObj.GetID(),
		"file_name": newName,
	}
	_, err := d.request("/file/rename", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *QuarkOrUC) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *QuarkOrUC) Remove(ctx context.Context, obj model.Obj) error {
	data := base.Json{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{obj.GetID()},
	}
	_, err := d.request("/file/delete", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *QuarkOrUC) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	tempFile, err := stream.CacheFullInTempFile()
	if err != nil {
		return err
	}
	defer func() {
		_ = tempFile.Close()
	}()
	m := md5.New()
	_, err = utils.CopyWithBuffer(m, tempFile)
	if err != nil {
		return err
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}
	md5Str := hex.EncodeToString(m.Sum(nil))
	s := sha1.New()
	_, err = utils.CopyWithBuffer(s, tempFile)
	if err != nil {
		return err
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}
	sha1Str := hex.EncodeToString(s.Sum(nil))
	// pre
	pre, err := d.upPre(stream, dstDir.GetID())
	if err != nil {
		return err
	}
	log.Debugln("hash: ", md5Str, sha1Str)
	// hash
	finish, err := d.upHash(md5Str, sha1Str, pre.Data.TaskId)
	if err != nil {
		return err
	}
	if finish {
		return nil
	}
	// part up
	partSize := pre.Metadata.PartSize
	var bytes []byte
	md5s := make([]string, 0)
	defaultBytes := make([]byte, partSize)
	total := stream.GetSize()
	left := total
	partNumber := 1
	for left > 0 {
		if utils.IsCanceled(ctx) {
			return ctx.Err()
		}
		if left > int64(partSize) {
			bytes = defaultBytes
		} else {
			bytes = make([]byte, left)
		}
		_, err := io.ReadFull(tempFile, bytes)
		if err != nil {
			return err
		}
		left -= int64(len(bytes))
		log.Debugf("left: %d", left)
		m, err := d.upPart(ctx, pre, stream.GetMimetype(), partNumber, bytes)
		//m, err := driver.UpPart(pre, file.GetMIMEType(), partNumber, bytes, account, md5Str, sha1Str)
		if err != nil {
			return err
		}
		if m == "finish" {
			return nil
		}
		md5s = append(md5s, m)
		partNumber++
		up(100 * float64(total-left) / float64(total))
	}
	err = d.upCommit(pre, md5s)
	if err != nil {
		return err
	}
	return d.upFinish(pre)
}

var _ driver.Driver = (*QuarkOrUC)(nil)
