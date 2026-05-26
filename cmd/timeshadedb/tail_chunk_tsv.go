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
	InputPath    string
	SavefileUUID string
	BaseURL      string
	BatchRows    int
	PollInterval time.Duration
	Once         bool
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
	baseURL, err := normalizeBaseURL(opts.BaseURL)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	meta, err := fetchIngestMetadata(ctx, client, baseURL, opts.SavefileUUID)
	if err != nil {
		return err
	}
	offset, err := catchUpChunkTSV(ctx, client, opts, baseURL, meta)
	if err != nil {
		return err
	}
	if opts.Once {
		return nil
	}
	return watchChunkTSV(ctx, client, opts, baseURL, offset)
}

func catchUpChunkTSV(ctx context.Context, client *http.Client, opts tailChunkTSVOptions, baseURL string, meta *timeshadedb.IngestMetadata) (int64, error) {
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
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 8192), 1024*1024)
	var batch []string
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := scanner.Text()
		lineBytes := int64(len(line)) + 1
		nextOffset := offset + lineBytes
		row, parseErr := timeshadedb.ParseChunkTSVRow(opts.SavefileUUID, line)
		if parseErr != nil {
			return 0, fmt.Errorf("%s:%d: %w", opts.InputPath, lineNo, parseErr)
		}
		if len(targets) > 0 && matchesResumeTarget(row, targets) {
			lastMatchedOffset = nextOffset
			batch = batch[:0]
		} else if len(targets) == 0 || lastMatchedOffset >= 0 {
			batch = append(batch, line)
			if len(batch) >= opts.BatchRows {
				if err := postChunkRows(ctx, client, baseURL, opts.SavefileUUID, batch); err != nil {
					return 0, err
				}
				batch = batch[:0]
			}
		}
		offset = nextOffset
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if len(batch) > 0 {
		if err := postChunkRows(ctx, client, baseURL, opts.SavefileUUID, batch); err != nil {
			return 0, err
		}
	}
	return offset, nil
}

func watchChunkTSV(ctx context.Context, client *http.Client, opts tailChunkTSVOptions, baseURL string, offset int64) error {
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := sendAppendedChunkRows(ctx, client, opts, baseURL, offset)
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

func sendAppendedChunkRows(ctx context.Context, client *http.Client, opts tailChunkTSVOptions, baseURL string, offset int64) (int64, error) {
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
		return catchUpChunkTSV(ctx, client, opts, baseURL, meta)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	reader := bufio.NewReader(f)
	var batch []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return offset, err
		}
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(trimmed) != "" {
				if _, parseErr := timeshadedb.ParseChunkTSVRow(opts.SavefileUUID, trimmed); parseErr != nil {
					return offset, parseErr
				}
				batch = append(batch, trimmed)
				if len(batch) >= opts.BatchRows {
					if err := postChunkRows(ctx, client, baseURL, opts.SavefileUUID, batch); err != nil {
						return offset, err
					}
					batch = batch[:0]
				}
			}
			offset += int64(len(line))
		}
		if err == io.EOF {
			break
		}
	}
	if len(batch) > 0 {
		if err := postChunkRows(ctx, client, baseURL, opts.SavefileUUID, batch); err != nil {
			return offset, err
		}
	}
	return offset, nil
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

func postChunkRows(ctx context.Context, client *http.Client, baseURL, savefileUUID string, rows []string) error {
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
		return postChunkRows(ctx, client, baseURL, savefileUUID, rows)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ingest request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
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
