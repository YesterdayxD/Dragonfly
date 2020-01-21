/*
 * Copyright The Dragonfly Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package downloader

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"

	"github.com/dragonflyoss/Dragonfly/dfget/config"
	"github.com/dragonflyoss/Dragonfly/dfget/core/downloader"
	"github.com/dragonflyoss/Dragonfly/dfget/core/regist"
	"github.com/dragonflyoss/Dragonfly/pkg/fileutils"
	"github.com/dragonflyoss/Dragonfly/pkg/httputils"
	"github.com/dragonflyoss/Dragonfly/pkg/limitreader"
	"github.com/dragonflyoss/Dragonfly/pkg/netutils"
	"github.com/dragonflyoss/Dragonfly/pkg/printer"
	"github.com/dragonflyoss/Dragonfly/pkg/stringutils"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// BackDownloader downloads the file from file resource.
type BackDownloader struct {
	// URL is the source url of the file to download.
	URL string

	// Target is the full target path.
	Target string

	// Md5 is the expected file md5 to prevent files from being tampered with.
	Md5 string

	// TaskID a string which represents a unique task.
	TaskID string

	cfg *config.Config

	tempFileName string
	cleaned      bool
}

var _ downloader.Downloader = &BackDownloader{}

// NewBackDownloader creates a BackDownloader.
func NewBackDownloader(cfg *config.Config, result *regist.RegisterResult) *BackDownloader {
	var (
		taskID string
	)
	if result != nil {
		taskID = result.TaskID
	}
	return &BackDownloader{
		cfg:    cfg,
		URL:    cfg.URL,
		Target: cfg.RV.RealTarget,
		Md5:    cfg.Md5,
		TaskID: taskID,
	}
}

// Run starts to download the file.
func (bd *BackDownloader) Run(ctx context.Context) error {
	var (
		resp *http.Response
		err  error
		f    *os.File
	)

	if bd.cfg.Notbs || bd.cfg.BackSourceReason == config.BackSourceReasonNoSpace {
		bd.cfg.BackSourceReason += config.ForceNotBackSourceAddition
		err = fmt.Errorf("download fail and not back source: %d", bd.cfg.BackSourceReason)
		return err
	}

	printer.Printf("start download %s from the source station", filepath.Base(bd.Target))
	logrus.Infof("start download %s from the source station", filepath.Base(bd.Target))

	defer bd.Cleanup()

	prefix := "backsource." + bd.cfg.Sign + "."
	if f, err = ioutil.TempFile(filepath.Dir(bd.Target), prefix); err != nil {
		return err
	}
	bd.tempFileName = f.Name()
	defer f.Close()

	if resp, err = httputils.HTTPGetWithTLS(bd.URL, netutils.ConvertHeaders(bd.cfg.Header), 0, bd.cfg.Cacerts, bd.cfg.Insecure); err != nil {
		return err
	}
	defer resp.Body.Close()

	if !bd.isSuccessStatus(resp.StatusCode) {
		return fmt.Errorf("failed to download from source, response code:%d", resp.StatusCode)
	}

	buf := make([]byte, 512*1024)
	reader := limitreader.NewLimitReader(resp.Body, int64(bd.cfg.LocalLimit), bd.Md5 != "")
	if _, err = io.CopyBuffer(f, reader, buf); err != nil {
		return err
	}

	realMd5 := reader.Md5()
	if bd.Md5 == "" || bd.Md5 == realMd5 {
		err = downloader.MoveFile(bd.tempFileName, bd.Target, "")
	} else {
		err = fmt.Errorf("md5 not match, expected:%s real:%s", bd.Md5, realMd5)
	}
	return err
}

// RunStream return a io.Reader without any disk io.
func (bd *BackDownloader) RunStream(ctx context.Context) (io.Reader, error) {
	var (
		resp *http.Response
		err  error
	)

	if bd.cfg.Notbs || bd.cfg.BackSourceReason == config.BackSourceReasonNoSpace {
		bd.cfg.BackSourceReason += config.ForceNotBackSourceAddition
		err = fmt.Errorf("download fail and not back source: %d", bd.cfg.BackSourceReason)
		return nil, err
	}

	if resp, err = httputils.HTTPGetWithTLS(bd.URL, netutils.ConvertHeaders(bd.cfg.Header), 0, bd.cfg.Cacerts, bd.cfg.Insecure); err != nil {
		return nil, err
	}

	if !bd.isSuccessStatus(resp.StatusCode) {
		return nil, fmt.Errorf("failed to download from source, response code:%d", resp.StatusCode)
	}

	limitReader := limitreader.NewLimitReader(resp.Body, int64(bd.cfg.LocalLimit), bd.Md5 != "")
	return &autoCloseLimitReader{closer: resp.Body, limitReader: limitReader, md5: bd.Md5}, nil
}

// Cleanup clean all temporary resources generated by executing Run.
func (bd *BackDownloader) Cleanup() {
	if bd.cleaned {
		return
	}

	if !stringutils.IsEmptyStr(bd.tempFileName) {
		fileutils.DeleteFile(bd.tempFileName)
	}
	bd.cleaned = true
}

func (bd *BackDownloader) isSuccessStatus(code int) bool {
	return code < 400
}

// autoCloseLimitReader will auto close when reader return a error(include io.EOF).
// it is necessary when return http.Response.Body as an io.Reader.
type autoCloseLimitReader struct {
	closer      io.Closer
	md5         string
	limitReader *limitreader.LimitReader
}

func (a *autoCloseLimitReader) Read(p []byte) (n int, err error) {
	n, err = a.limitReader.Read(p)
	// when return err, always close
	if err != nil {
		if closeError := a.closer.Close(); closeError != nil {
			err = errors.Wrapf(err, "close error: %s", closeError)
		}
	}
	// all data received, calculate md5
	if err == io.EOF && a.md5 != "" {
		realMd5 := a.limitReader.Md5()
		if realMd5 != a.md5 {
			return n, fmt.Errorf("md5 not match, expected: %s real: %s", a.md5, realMd5)
		}
	}
	return n, err
}
