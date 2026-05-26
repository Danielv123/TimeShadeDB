package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"timeshadedb"
)

type tailChunkTSVOptions struct {
	InputPath        string
	SavefileUUID     string
	BaseURL          string
	BatchRows        int
	PollInterval     time.Duration
	ProgressInterval time.Duration
	RequestTimeout   time.Duration
	ProgressOutput   io.Writer
	Once             bool
}

type resumeTarget struct {
	tick    uint64
	surface string
	chunk   timeshadedb.ChunkCoord
	force   string
}

func tailChunkTSV(ctx context.Context, opts tailChunkTSVOptions) error {
	if opts.BatchRows <= 0 {
		return fmt.Errorf("batch-rows must be positive")
	}
	if opts.PollInterval <= 0 {
		return fmt.Errorf("poll-interval must be positive")
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 5 * time.Minute
	}
	reporter := newChunkProgressReporter(opts.ProgressOutput, opts.ProgressInterval)
	baseURL, err := normalizeBaseURL(opts.BaseURL)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: opts.RequestTimeout}
	meta, err := fetchIngestMetadata(ctx, client, baseURL, opts.SavefileUUID)
	if err != nil {
		return err
	}
	offset, err := catchUpChunkTSV(ctx, client, opts, baseURL, meta, reporter)
	if err != nil {
		return err
	}
	reporter.reportFinal()
	if opts.Once {
		return nil
	}
	return watchChunkTSV(ctx, client, opts, baseURL, offset, reporter)
}

func catchUpChunkTSV(ctx context.Context, client *http.Client, opts tailChunkTSVOptions, baseURL string, meta *timeshadedb.IngestMetadata, reporter *chunkProgressReporter) (int64, error) {
	f, err := os.Open(opts.InputPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	targets := resumeTargets(meta)
	var offset int64
	var lastMatchedOffset int64
	if len(targets) > 0 {
		lastMatchedOffset = -1
	}
	var batch []string
	reader := bufio.NewReader(f)
	for lineNo := 1; ; lineNo++ {
		line, lineBytes, complete, err := readChunkTSVLine(reader)
		if err != nil {
			return 0, err
		}
		if lineBytes == 0 {
			break
		}
		if !complete && !opts.Once {
			break
		}
		nextOffset := offset + lineBytes
		if len(targets) > 0 {
			row, parseErr := timeshadedb.ParseChunkTSVRow(opts.SavefileUUID, line)
			if parseErr != nil {
				return 0, fmt.Errorf("%s:%d: %w", opts.InputPath, lineNo, parseErr)
			}
			if matchesResumeTarget(row, targets) {
				lastMatchedOffset = nextOffset
				batch = batch[:0]
				offset = nextOffset
				continue
			}
		}
		if len(targets) == 0 || lastMatchedOffset >= 0 {
			batch = append(batch, line)
			if len(batch) >= opts.BatchRows {
				if err := postChunkRows(ctx, client, baseURL, opts.SavefileUUID, batch, reporter); err != nil {
					return 0, err
				}
				batch = batch[:0]
			}
		}
		offset = nextOffset
	}
	if len(batch) > 0 {
		if err := postChunkRows(ctx, client, baseURL, opts.SavefileUUID, batch, reporter); err != nil {
			return 0, err
		}
	}
	return offset, nil
}

func watchChunkTSV(ctx context.Context, client *http.Client, opts tailChunkTSVOptions, baseURL string, offset int64, reporter *chunkProgressReporter) error {
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := sendAppendedChunkRows(ctx, client, opts, baseURL, offset, reporter)
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

func sendAppendedChunkRows(ctx context.Context, client *http.Client, opts tailChunkTSVOptions, baseURL string, offset int64, reporter *chunkProgressReporter) (int64, error) {
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
		meta, err := fetchIngestMetadata(ctx, client, baseURL, opts.SavefileUUID)
		if err != nil {
			return offset, err
		}
		return catchUpChunkTSV(ctx, client, opts, baseURL, meta, reporter)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	reader := bufio.NewReader(f)
	var batch []string
	for {
		line, lineBytes, complete, err := readChunkTSVLine(reader)
		if err != nil {
			return offset, err
		}
		if lineBytes == 0 {
			break
		}
		if !complete {
			break
		}
		if strings.TrimSpace(line) != "" {
			batch = append(batch, line)
			if len(batch) >= opts.BatchRows {
				if err := postChunkRows(ctx, client, baseURL, opts.SavefileUUID, batch, reporter); err != nil {
					return offset, err
				}
				batch = batch[:0]
			}
		}
		offset += lineBytes
	}
	if len(batch) > 0 {
		if err := postChunkRows(ctx, client, baseURL, opts.SavefileUUID, batch, reporter); err != nil {
			return offset, err
		}
	}
	return offset, nil
}

func readChunkTSVLine(reader *bufio.Reader) (line string, bytesRead int64, complete bool, err error) {
	raw, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", 0, false, err
	}
	if raw == "" {
		return "", 0, false, nil
	}
	complete = strings.HasSuffix(raw, "\n")
	if err == io.EOF && !complete {
		return strings.TrimRight(raw, "\r\n"), int64(len(raw)), false, nil
	}
	return strings.TrimRight(raw, "\r\n"), int64(len(raw)), complete, nil
}

func fetchIngestMetadata(ctx context.Context, client *http.Client, baseURL, savefileUUID string) (*timeshadedb.IngestMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/ingest/chunk/"+url.PathEscape(savefileUUID)+"/meta", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("metadata request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var meta timeshadedb.IngestMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func postChunkRows(ctx context.Context, client *http.Client, baseURL, savefileUUID string, rows []string, reporter *chunkProgressReporter) error {
	body := bytes.NewBuffer(nil)
	for _, row := range rows {
		body.WriteString(row)
		body.WriteByte('\n')
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/ingest/chunk/"+url.PathEscape(savefileUUID), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/tab-separated-values")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		time.Sleep(500 * time.Millisecond)
		return postChunkRows(ctx, client, baseURL, savefileUUID, rows, reporter)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ingest request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	reporter.record(len(rows))
	return nil
}

type chunkProgressReporter struct {
	out            io.Writer
	interval       time.Duration
	start          time.Time
	lastReport     time.Time
	lastReportRows uint64
	totalRows      uint64
}

func newChunkProgressReporter(out io.Writer, interval time.Duration) *chunkProgressReporter {
	if out == nil {
		out = os.Stderr
	}
	now := time.Now()
	return &chunkProgressReporter{out: out, interval: interval, start: now, lastReport: now}
}

func (r *chunkProgressReporter) record(rows int) {
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

func (r *chunkProgressReporter) reportFinal() {
	if r == nil || r.totalRows == 0 {
		return
	}
	r.report(time.Now(), true)
}

func (r *chunkProgressReporter) report(now time.Time, final bool) {
	windowRows := r.totalRows - r.lastReportRows
	windowMinutes := now.Sub(r.lastReport).Minutes()
	totalMinutes := now.Sub(r.start).Minutes()
	windowRate := chunksPerMinute(windowRows, windowMinutes)
	averageRate := chunksPerMinute(r.totalRows, totalMinutes)
	label := "progress"
	if final {
		label = "summary"
	}
	fmt.Fprintf(r.out, "tail-chunk-tsv %s: pushed %d chunks total, %.1f chunks/min current, %.1f chunks/min average\n", label, r.totalRows, windowRate, averageRate)
	r.lastReport = now
	r.lastReportRows = r.totalRows
}

func chunksPerMinute(rows uint64, minutes float64) float64 {
	if rows == 0 || minutes <= 0 {
		return 0
	}
	return float64(rows) / minutes
}

func resumeTargets(meta *timeshadedb.IngestMetadata) map[resumeTarget]struct{} {
	targets := map[resumeTarget]struct{}{}
	if meta == nil {
		return targets
	}
	for _, ds := range meta.Datastores {
		if ds.LatestRowSeq == 0 {
			continue
		}
		targets[resumeTarget{
			tick:    ds.LatestTick,
			surface: ds.Surface,
			chunk:   timeshadedb.ChunkCoord{X: ds.LatestChunkX, Y: ds.LatestChunkY},
			force:   ds.Force,
		}] = struct{}{}
	}
	return targets
}

func matchesResumeTarget(row timeshadedb.ParsedChunkRow, targets map[resumeTarget]struct{}) bool {
	_, ok := targets[resumeTarget{
		tick:    row.Tick,
		surface: row.Key.Surface,
		chunk:   row.Chunk,
		force:   row.Key.Force,
	}]
	return ok
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid base URL %q", raw)
	}
	return raw, nil
}
