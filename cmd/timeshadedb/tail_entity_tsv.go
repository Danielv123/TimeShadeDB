package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"timeshadedb"
)

type tailEntityTSVOptions struct {
	InputPath          string
	SavefileUUID       string
	VictoriaMetricsURL string
	BatchRows          int
	PollInterval       time.Duration
	ProgressInterval   time.Duration
	RequestTimeout     time.Duration
	RetryInterval      time.Duration
	ProgressOutput     io.Writer
	Once               bool
}

func tailEntityTSV(ctx context.Context, opts tailEntityTSVOptions) error {
	if opts.InputPath == "" {
		return fmt.Errorf("entity input path is required")
	}
	if opts.SavefileUUID == "" {
		return fmt.Errorf("savefile UUID is required")
	}
	if opts.VictoriaMetricsURL == "" {
		return fmt.Errorf("victoriametrics URL is required")
	}
	if opts.BatchRows <= 0 {
		return fmt.Errorf("batch-rows must be positive")
	}
	if opts.PollInterval <= 0 {
		return fmt.Errorf("poll-interval must be positive")
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 5 * time.Minute
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = 5 * time.Second
	}
	importURL, err := normalizeVictoriaMetricsImportURL(opts.VictoriaMetricsURL)
	if err != nil {
		return err
	}
	reporter := newEntityProgressReporter(opts.ProgressOutput, opts.ProgressInterval)
	client := &http.Client{Timeout: opts.RequestTimeout}
	offset, err := sendEntityFileFromOffset(ctx, client, opts, importURL, 0, reporter, !opts.Once)
	if err != nil {
		return err
	}
	reporter.reportFinal()
	if opts.Once {
		return nil
	}
	return watchEntityTSV(ctx, client, opts, importURL, offset, reporter)
}

func watchEntityTSV(ctx context.Context, client *http.Client, opts tailEntityTSVOptions, importURL string, offset int64, reporter *entityProgressReporter) error {
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := sendEntityFileFromOffset(ctx, client, opts, importURL, offset, reporter, true)
		if err != nil {
			return err
		}
		offset = next
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func sendEntityFileFromOffset(ctx context.Context, client *http.Client, opts tailEntityTSVOptions, importURL string, offset int64, reporter *entityProgressReporter, deferPartial bool) (int64, error) {
	f, err := os.Open(opts.InputPath)
	if err != nil {
		return offset, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if info.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	reader := bufio.NewReader(f)
	var batch []timeshadedb.EntityPoint
	for lineNo := 1; ; lineNo++ {
		line, lineBytes, complete, err := readChunkTSVLine(reader)
		if err != nil {
			return offset, err
		}
		if lineBytes == 0 {
			break
		}
		if !complete && deferPartial {
			break
		}
		nextOffset := offset + lineBytes
		if strings.TrimSpace(line) != "" {
			point, err := timeshadedb.ParseEntityTSVRow(opts.SavefileUUID, line)
			if err != nil {
				return offset, fmt.Errorf("%s:%d: %w", opts.InputPath, lineNo, err)
			}
			batch = append(batch, point)
			if len(batch) >= opts.BatchRows {
				if err := postEntityPoints(ctx, client, importURL, batch, reporter, opts.RetryInterval); err != nil {
					return offset, err
				}
				batch = batch[:0]
			}
		}
		offset = nextOffset
	}
	if len(batch) > 0 {
		if err := postEntityPoints(ctx, client, importURL, batch, reporter, opts.RetryInterval); err != nil {
			return offset, err
		}
	}
	return offset, nil
}

func postEntityPoints(ctx context.Context, client *http.Client, importURL string, points []timeshadedb.EntityPoint, reporter *entityProgressReporter, retryInterval time.Duration) error {
	body := timeshadedb.FormatEntityPrometheus(points)
	for {
		err := postEntityPrometheusOnce(ctx, client, importURL, body)
		if err == nil {
			reporter.record(len(points))
			return nil
		}
		if !isRetryableIngestError(err) {
			return err
		}
		if err := waitForRetry(ctx, retryInterval); err != nil {
			return err
		}
	}
}

func postEntityPrometheusOnce(ctx context.Context, client *http.Client, importURL string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, importURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	resp, err := client.Do(req)
	if err != nil {
		return retryableIngestError{err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return retryableIngestError{err: fmt.Errorf("entity ingest request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("entity ingest request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func normalizeVictoriaMetricsImportURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid VictoriaMetrics URL %q", raw)
	}
	if u.Path == "" {
		u.Path = "/api/v1/import/prometheus"
		return u.String(), nil
	}
	if strings.HasSuffix(u.Path, "/api/v1/import/prometheus") || strings.HasSuffix(u.Path, "/prometheus/api/v1/import/prometheus") {
		return u.String(), nil
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/import/prometheus"
	return u.String(), nil
}

type entityProgressReporter struct {
	out            io.Writer
	interval       time.Duration
	start          time.Time
	lastReport     time.Time
	lastReportRows uint64
	totalRows      uint64
}

func newEntityProgressReporter(out io.Writer, interval time.Duration) *entityProgressReporter {
	if out == nil {
		out = os.Stderr
	}
	now := time.Now()
	return &entityProgressReporter{out: out, interval: interval, start: now, lastReport: now}
}

func (r *entityProgressReporter) record(rows int) {
	if r == nil || rows <= 0 {
		return
	}
	r.totalRows += uint64(rows)
	if r.interval <= 0 {
		return
	}
	now := time.Now()
	if now.Sub(r.lastReport) >= r.interval {
		r.report(now, false)
	}
}

func (r *entityProgressReporter) reportFinal() {
	if r == nil || r.totalRows == 0 {
		return
	}
	r.report(time.Now(), true)
}

func (r *entityProgressReporter) report(now time.Time, final bool) {
	windowRows := r.totalRows - r.lastReportRows
	windowMinutes := now.Sub(r.lastReport).Minutes()
	totalMinutes := now.Sub(r.start).Minutes()
	windowRate := chunksPerMinute(windowRows, windowMinutes)
	averageRate := chunksPerMinute(r.totalRows, totalMinutes)
	label := "progress"
	if final {
		label = "summary"
	}
	fmt.Fprintf(r.out, "tail-entity-tsv %s: pushed %d entity points total, %.1f points/min current, %.1f points/min average\n", label, r.totalRows, windowRate, averageRate)
	r.lastReport = now
	r.lastReportRows = r.totalRows
}
