package quark

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	log "github.com/sirupsen/logrus"
)

const (
	uploadControlMaxAttempts             = 3
	uploadControlInitialBackoff          = time.Second
	uploadControlMaxBackoff              = 2 * time.Second
	uploadFinishVisibilityAttempts       = 4
	uploadFinishVisibilityInitialBackoff = 250 * time.Millisecond
	uploadFinishVisibilityMaxBackoff     = time.Second
)

func wrapQuarkUploadStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("quark upload stage %s: %w", stage, err)
}

func isRetryableQuarkUploadControlError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.HasPrefix(msg, "complete_upload_lock_timeout") {
		return true
	}
	return strings.HasPrefix(msg, "inner error") && strings.Contains(msg, "requestid")
}

func waitWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryQuarkUploadControl[T any](ctx context.Context, stage string, fn func() (T, error)) (T, error) {
	var zero T
	backoff := uploadControlInitialBackoff
	for attempt := 1; attempt <= uploadControlMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, wrapQuarkUploadStage(stage, err)
		}

		value, err := fn()
		if err == nil {
			return value, nil
		}
		if !isRetryableQuarkUploadControlError(err) {
			return zero, wrapQuarkUploadStage(stage, err)
		}
		if attempt == uploadControlMaxAttempts {
			return zero, wrapQuarkUploadStage(stage, fmt.Errorf("transient provider error after %d attempts: %w", attempt, err))
		}

		log.Warnf("quark upload stage=%s transient provider error attempt=%d/%d: %v; retrying", stage, attempt, uploadControlMaxAttempts, err)
		if err := waitWithContext(ctx, backoff); err != nil {
			return zero, wrapQuarkUploadStage(stage, err)
		}
		backoff *= 2
		if backoff > uploadControlMaxBackoff {
			backoff = uploadControlMaxBackoff
		}
	}
	return zero, wrapQuarkUploadStage(stage, errorsNewUnreachable())
}

// errorsNewUnreachable keeps the generic retry helper total without exposing a
// provider-specific sentinel. The loop above always returns before this point.
func errorsNewUnreachable() error {
	return fmt.Errorf("retry loop exhausted unexpectedly")
}

func (d *QuarkOrUC) upPreReliable(ctx context.Context, file model.FileStreamer, parentID string) (UpPreResp, error) {
	if err := ctx.Err(); err != nil {
		return UpPreResp{}, wrapQuarkUploadStage("pre", err)
	}
	pre, err := d.upPre(file, parentID)
	if err != nil {
		return pre, wrapQuarkUploadStage("pre", err)
	}
	return pre, nil
}

func (d *QuarkOrUC) upHashReliable(ctx context.Context, md5, sha1, taskID string) (bool, error) {
	return retryQuarkUploadControl(ctx, "hash", func() (bool, error) {
		return d.upHash(md5, sha1, taskID)
	})
}

func (d *QuarkOrUC) upPartReliable(ctx context.Context, pre UpPreResp, mimeType string, partNumber int, part []byte) (string, error) {
	stage := fmt.Sprintf("part[%d]", partNumber)
	return retryQuarkUploadControl(ctx, stage, func() (string, error) {
		// Rebuild the reader for every attempt. A retry is only admitted for the
		// exact Quark control-plane transient errors returned by upload/auth,
		// before the OSS body is consumed.
		reader := driver.NewLimitedUploadStream(ctx, bytes.NewReader(part))
		return d.upPart(ctx, pre, mimeType, partNumber, io.Reader(reader))
	})
}

func (d *QuarkOrUC) upCommitReliable(ctx context.Context, pre UpPreResp, md5s []string) error {
	_, err := retryQuarkUploadControl(ctx, "commit", func() (struct{}, error) {
		// upCommit requests upload/auth before issuing CompleteMultipartUpload.
		// The retry predicate only accepts the raw Quark control-plane transient
		// messages, so OSS commit failures are never replayed here.
		return struct{}{}, d.upCommit(pre, md5s)
	})
	return err
}

func (d *QuarkOrUC) upFinishReliable(ctx context.Context, pre UpPreResp, parentID, fileName string, fileSize int64) error {
	backoff := uploadControlInitialBackoff
	for attempt := 1; attempt <= uploadControlMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return wrapQuarkUploadStage("finish", err)
		}

		err := d.upFinish(pre)
		if err == nil {
			return nil
		}
		if !isRetryableQuarkUploadControlError(err) {
			return wrapQuarkUploadStage("finish", err)
		}

		visible, visibilityErr := d.waitUploadedObjectVisible(ctx, pre, parentID, fileName, fileSize)
		if visible {
			log.Warnf("quark upload stage=finish returned transient provider error but uploaded object is visible; treating as success: %v", err)
			return nil
		}
		if visibilityErr != nil {
			log.Warnf("quark upload stage=finish visibility verification failed after provider error: %v", visibilityErr)
		}
		if attempt == uploadControlMaxAttempts {
			return wrapQuarkUploadStage("finish", fmt.Errorf("transient provider error after %d attempts: %w", attempt, err))
		}

		log.Warnf("quark upload stage=finish transient provider error attempt=%d/%d: %v; target not yet visible, retrying finish", attempt, uploadControlMaxAttempts, err)
		if err := waitWithContext(ctx, backoff); err != nil {
			return wrapQuarkUploadStage("finish", err)
		}
		backoff *= 2
		if backoff > uploadControlMaxBackoff {
			backoff = uploadControlMaxBackoff
		}
	}
	return wrapQuarkUploadStage("finish", errorsNewUnreachable())
}

func (d *QuarkOrUC) waitUploadedObjectVisible(ctx context.Context, pre UpPreResp, parentID, fileName string, fileSize int64) (bool, error) {
	backoff := uploadFinishVisibilityInitialBackoff
	var lastErr error
	for attempt := 0; attempt < uploadFinishVisibilityAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		files, err := d.GetFiles(parentID)
		if err != nil {
			lastErr = err
		} else {
			lastErr = nil
			for _, obj := range files {
				if obj.IsDir() {
					continue
				}
				if pre.Data.Fid != "" {
					if obj.GetID() == pre.Data.Fid {
						return true, nil
					}
					continue
				}
				if obj.GetName() == fileName && obj.GetSize() == fileSize {
					return true, nil
				}
			}
		}

		if attempt == uploadFinishVisibilityAttempts-1 {
			break
		}
		if err := waitWithContext(ctx, backoff); err != nil {
			return false, err
		}
		backoff *= 2
		if backoff > uploadFinishVisibilityMaxBackoff {
			backoff = uploadFinishVisibilityMaxBackoff
		}
	}
	return false, lastErr
}
