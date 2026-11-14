package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/httpapi"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/media"
	appservice "github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/service"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/storage"
)

type checkContext struct {
	root   string
	server *httptest.Server
	client *http.Client
	audio  []byte
	video  []byte
}

func main() {
	workflow := flag.String("workflow", "", "workflow to exercise")
	flag.Parse()
	if *workflow == "" {
		fatalf("workflow is required")
	}
	ctx, err := newCheckContext()
	if err != nil {
		fatalf("prepare check: %v", err)
	}
	defer ctx.server.Close()
	defer os.RemoveAll(ctx.root)

	switch *workflow {
	case "admit-take":
		err = runAdmitCheck(ctx)
	case "analyze-sync":
		err = runAnalyzeCheck(ctx)
	case "render-conform":
		err = runRenderCheck(ctx)
	case "manual-conform":
		err = runManualConformCheck(ctx)
	default:
		err = fmt.Errorf("unknown workflow %q", *workflow)
	}
	if err != nil {
		fatalf("%s failed: %v", *workflow, err)
	}
	fmt.Printf("%s workflow verified\n", *workflow)
}

func newCheckContext() (*checkContext, error) {
	root, err := os.MkdirTemp("", "sync-conformer-workflow-*")
	if err != nil {
		return nil, err
	}
	repository, err := storage.New(root)
	if err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	audio, video, err := media.CalibrationPair()
	if err != nil {
		os.RemoveAll(root)
		return nil, err
	}
	api := httpapi.New(appservice.New(repository))
	server := httptest.NewServer(api.Handler())
	return &checkContext{
		root:   root,
		server: server,
		client: &http.Client{Timeout: 20 * time.Second},
		audio:  audio,
		video:  video,
	}, nil
}

func runAdmitCheck(ctx *checkContext) error {
	racePayload := func(name string) map[string]any {
		return takePayload("race-take-01", name, ctx.audio, ctx.video)
	}
	statuses := make([]int, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		response, _, err := ctx.requestJSON(http.MethodPost, "/v1/takes", racePayload("camera left"))
		if err != nil {
			statuses[0] = 0
			return
		}
		statuses[0] = response.StatusCode
	}()
	go func() {
		defer wait.Done()
		response, _, err := ctx.requestJSON(http.MethodPost, "/v1/takes", racePayload("camera right"))
		if err != nil {
			statuses[1] = 0
			return
		}
		statuses[1] = response.StatusCode
	}()
	wait.Wait()
	sort.Ints(statuses)
	if statuses[0] != http.StatusCreated || statuses[1] != http.StatusConflict {
		return fmt.Errorf("identifier race returned statuses %v", statuses)
	}

	response, body, err := ctx.requestJSON(
		http.MethodPost,
		"/v1/takes",
		takePayload("stable-take-01", "stable take", ctx.audio, ctx.video),
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("first admission returned %d: %s", response.StatusCode, body)
	}
	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/takes",
		takePayload("stable-take-01", "stable take", ctx.audio, ctx.video),
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("identical retry returned %d: %s", response.StatusCode, body)
	}

	response, body, err = ctx.requestJSON(http.MethodGet, "/v1/takes/stable-take-01", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("take read returned %d: %s", response.StatusCode, body)
	}
	var admitted domain.MediaTake
	if err := json.Unmarshal(body, &admitted); err != nil {
		return fmt.Errorf("decode admitted take: %w", err)
	}
	if admitted.Digest == "" || admitted.Video.FrameCount != media.CalibrationFrames {
		return fmt.Errorf("admitted take descriptor is incomplete")
	}

	badAudio := base64.StdEncoding.EncodeToString([]byte("not a wave payload"))
	badPayload := takePayload("bad-audio-01", "bad audio", ctx.audio, ctx.video)
	badPayload["audio_base64"] = badAudio
	response, body, err = ctx.requestJSON(http.MethodPost, "/v1/takes", badPayload)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusUnprocessableEntity {
		return fmt.Errorf("malformed audio returned %d: %s", response.StatusCode, body)
	}
	if code := errorCode(body); code != "invalid_audio" {
		return fmt.Errorf("malformed audio returned code %q", code)
	}
	persisted, err := storage.New(ctx.root)
	if err != nil {
		return err
	}
	if _, err := persisted.GetTake("stable-take-01"); err != nil {
		return fmt.Errorf("persisted take is unreadable: %w", err)
	}
	return nil
}

func runAnalyzeCheck(ctx *checkContext) error {
	if err := admitTake(ctx, "analysis-take-01"); err != nil {
		return err
	}
	response, body, err := ctx.requestJSON(
		http.MethodPost,
		"/v1/takes/analysis-take-01/sync-analyses",
		map[string]any{
			"analysis_id":    "analysis-check-01",
			"min_confidence": 0.55,
			"max_offset_ms":  1000,
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("analysis returned %d: %s", response.StatusCode, body)
	}
	var analysis domain.SyncAnalysis
	if err := json.Unmarshal(body, &analysis); err != nil {
		return fmt.Errorf("decode analysis: %w", err)
	}
	if analysis.State != domain.AnalysisStateReady {
		return fmt.Errorf("analysis state is %s: %s", analysis.State, analysis.ErrorMessage)
	}
	if analysis.OffsetMS == nil || *analysis.OffsetMS != 125 {
		return fmt.Errorf("analysis offset is %v, want 125", analysis.OffsetMS)
	}
	if analysis.AudioEventMS == nil || *analysis.AudioEventMS != 500 {
		return fmt.Errorf("audio event is %v, want 500", analysis.AudioEventMS)
	}
	if analysis.VideoEventMS == nil || *analysis.VideoEventMS != 375 {
		return fmt.Errorf("picture event is %v, want 375", analysis.VideoEventMS)
	}
	if analysis.Confidence < 0.55 {
		return fmt.Errorf("analysis confidence is %.3f", analysis.Confidence)
	}
	persisted, err := storage.New(ctx.root)
	if err != nil {
		return err
	}
	stored, err := persisted.GetAnalysis("analysis-check-01")
	if err != nil {
		return fmt.Errorf("read persisted analysis: %w", err)
	}
	if stored.State != domain.AnalysisStateReady || stored.OffsetMS == nil {
		return fmt.Errorf("persisted analysis is not terminal")
	}
	return nil
}

func runRenderCheck(ctx *checkContext) error {
	if err := admitTake(ctx, "conform-take-01"); err != nil {
		return err
	}
	if err := createAnalysis(ctx, "conform-take-01", "conform-analysis-01"); err != nil {
		return err
	}
	response, body, err := ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/conform-analysis-01/conforms",
		map[string]any{
			"conform_id":        "review-conform-01",
			"max_correction_ms": 250,
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("conform returned %d: %s", response.StatusCode, body)
	}
	var conform domain.ConformJob
	if err := json.Unmarshal(body, &conform); err != nil {
		return fmt.Errorf("decode conform: %w", err)
	}
	if conform.State != domain.ConformStateCompleted || conform.BundleDigest == "" {
		return fmt.Errorf("conform did not complete: %s", conform.ErrorMessage)
	}

	response, bundle, err := ctx.requestJSON(
		http.MethodGet,
		"/v1/conforms/review-conform-01/bundle",
		nil,
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("bundle read returned %d: %s", response.StatusCode, bundle)
	}
	entries, err := readZip(bundle)
	if err != nil {
		return err
	}
	for _, name := range []string{"audio-aligned.wav", "video-conformed.y4m", "conform-receipt.json"} {
		if len(entries[name]) == 0 {
			return fmt.Errorf("bundle entry %s is missing", name)
		}
	}
	measurement, err := media.AnalyzeSync(entries["audio-aligned.wav"], entries["video-conformed.y4m"])
	if err != nil {
		return fmt.Errorf("analyze aligned bundle: %w", err)
	}
	if measurement.OffsetMS < -2 || measurement.OffsetMS > 2 {
		return fmt.Errorf("aligned audio offset is %d ms", measurement.OffsetMS)
	}
	var receipt domain.ConformReceipt
	if err := json.Unmarshal(entries["conform-receipt.json"], &receipt); err != nil {
		return fmt.Errorf("decode receipt: %w", err)
	}
	if receipt.AppliedOffsetMS != 125 || receipt.InputTakeDigest == "" {
		return fmt.Errorf("receipt does not describe the applied correction")
	}

	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/conform-analysis-01/conforms",
		map[string]any{
			"conform_id":        "too-large-01",
			"max_correction_ms": 50,
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusUnprocessableEntity {
		return fmt.Errorf("oversized correction returned %d: %s", response.StatusCode, body)
	}
	response, body, err = ctx.requestJSON(http.MethodGet, "/v1/conforms/too-large-01/bundle", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusConflict {
		return fmt.Errorf("failed correction exposed a bundle: %d %s", response.StatusCode, body)
	}
	return nil
}

func runManualConformCheck(ctx *checkContext) error {
	if err := admitTake(ctx, "manual-take-01"); err != nil {
		return err
	}
	if err := createAnalysis(ctx, "manual-take-01", "manual-analysis-01"); err != nil {
		return err
	}

	// The automatic analysis record stays intact and available.
	response, body, err := ctx.requestJSON(http.MethodGet, "/v1/sync-analyses/manual-analysis-01", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("analysis read returned %d: %s", response.StatusCode, body)
	}
	var analysis domain.SyncAnalysis
	if err := json.Unmarshal(body, &analysis); err != nil {
		return fmt.Errorf("decode analysis: %w", err)
	}
	if analysis.State != domain.AnalysisStateReady || analysis.OffsetMS == nil || *analysis.OffsetMS != 125 {
		return fmt.Errorf("automatic analysis changed after a manual decision")
	}

	// Render the automatic bundle first so manual work cannot rewrite it.
	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/manual-analysis-01/conforms",
		map[string]any{
			"conform_id":        "auto-review-01",
			"max_correction_ms": 500,
			"decision_source":   "automatic",
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("automatic conform returned %d: %s", response.StatusCode, body)
	}
	var autoConform domain.ConformJob
	if err := json.Unmarshal(body, &autoConform); err != nil {
		return fmt.Errorf("decode automatic conform: %w", err)
	}
	if autoConform.DecisionSource != domain.DecisionSourceAutomatic ||
		autoConform.MeasuredOffsetMS != 125 || autoConform.DecidedOffsetMS != 125 ||
		autoConform.AppliedOffsetMS != 125 || autoConform.InputAudioDigest == "" {
		return fmt.Errorf("automatic conform does not separate basis from movement: %+v", autoConform)
	}
	autoBundleDigest := autoConform.BundleDigest

	// A separate manual bundle uses the operator's lip-sync decision while the
	// measured offset remains visible as the actual basis.
	manualOffset := -40
	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/manual-analysis-01/conforms",
		map[string]any{
			"conform_id":        "manual-review-01",
			"max_correction_ms": 500,
			"decision_source":   "manual",
			"manual_offset_ms":  manualOffset,
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("manual conform returned %d: %s", response.StatusCode, body)
	}
	var manualConform domain.ConformJob
	if err := json.Unmarshal(body, &manualConform); err != nil {
		return fmt.Errorf("decode manual conform: %w", err)
	}
	if manualConform.DecisionSource != domain.DecisionSourceManual {
		return fmt.Errorf("manual conform source is %q", manualConform.DecisionSource)
	}
	if manualConform.MeasuredOffsetMS != 125 {
		return fmt.Errorf("manual conform lost the measured basis: %d", manualConform.MeasuredOffsetMS)
	}
	if manualConform.ManualOffsetMS == nil || *manualConform.ManualOffsetMS != manualOffset {
		return fmt.Errorf("manual conform did not record the operator offset: %v", manualConform.ManualOffsetMS)
	}
	if manualConform.DecidedOffsetMS != manualOffset || manualConform.AppliedOffsetMS != manualOffset {
		return fmt.Errorf("manual conform applied %d instead of the decided %d",
			manualConform.AppliedOffsetMS, manualConform.DecidedOffsetMS)
	}
	if manualConform.InputAudioDigest != autoConform.InputAudioDigest {
		return fmt.Errorf("manual and automatic conforms pin different audio")
	}
	if manualConform.BundleDigest == "" || manualConform.BundleDigest == autoBundleDigest {
		return fmt.Errorf("manual bundle is not a distinct artifact")
	}

	response, bundle, err := ctx.requestJSON(http.MethodGet, "/v1/conforms/manual-review-01/bundle", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("manual bundle read returned %d: %s", response.StatusCode, bundle)
	}
	entries, err := readZip(bundle)
	if err != nil {
		return err
	}
	var receipt domain.ConformReceipt
	if err := json.Unmarshal(entries["conform-receipt.json"], &receipt); err != nil {
		return fmt.Errorf("decode manual receipt: %w", err)
	}
	if receipt.SchemaVersion != 2 ||
		receipt.DecisionSource != domain.DecisionSourceManual ||
		receipt.MeasuredOffsetMS != 125 ||
		receipt.ManualOffsetMS == nil || *receipt.ManualOffsetMS != manualOffset ||
		receipt.DecidedOffsetMS != manualOffset ||
		receipt.AppliedOffsetMS != manualOffset {
		return fmt.Errorf("manual receipt does not separate basis and movement: %+v", receipt)
	}
	aligned, err := media.AnalyzeSync(entries["audio-aligned.wav"], entries["video-conformed.y4m"])
	if err != nil {
		return fmt.Errorf("analyze manual bundle: %w", err)
	}
	// A -40 ms correction moves the original +125 ms residual toward +165 ms,
	// proving the actual movement matches the manual value rather than 125.
	if aligned.OffsetMS < 160 || aligned.OffsetMS > 170 {
		return fmt.Errorf("manual movement is wrong, residual offset %d ms", aligned.OffsetMS)
	}

	// The previously completed automatic bundle is byte-identical and still served.
	response, autoBundleNow, err := ctx.requestJSON(http.MethodGet, "/v1/conforms/auto-review-01/bundle", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("automatic bundle no longer readable: %d", response.StatusCode)
	}
	if media.DigestBytes(autoBundleNow) != autoBundleDigest {
		return fmt.Errorf("automatic bundle was rewritten by the manual decision")
	}
	response, body, err = ctx.requestJSON(http.MethodGet, "/v1/conforms/auto-review-01", nil)
	if err != nil {
		return err
	}
	var autoReadback domain.ConformJob
	if err := json.Unmarshal(body, &autoReadback); err != nil {
		return fmt.Errorf("decode automatic readback: %w", err)
	}
	if autoReadback.State != domain.ConformStateCompleted ||
		autoReadback.AppliedOffsetMS != 125 ||
		autoReadback.DecisionSource != domain.DecisionSourceAutomatic {
		return fmt.Errorf("completed automatic conform was modified")
	}

	// An identical manual request retries to the same record instead of rebuilding.
	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/manual-analysis-01/conforms",
		map[string]any{
			"conform_id":        "manual-review-01",
			"max_correction_ms": 500,
			"decision_source":   "manual",
			"manual_offset_ms":  manualOffset,
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("identical manual retry returned %d: %s", response.StatusCode, body)
	}
	var retried domain.ConformJob
	if err := json.Unmarshal(body, &retried); err != nil {
		return fmt.Errorf("decode retried conform: %w", err)
	}
	if retried.BundleDigest != manualConform.BundleDigest {
		return fmt.Errorf("identical retry rebuilt or switched the bundle")
	}

	// Reusing the same delivery id with a different manual value is a conflict,
	// not a silent replacement of audio/decision.
	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/manual-analysis-01/conforms",
		map[string]any{
			"conform_id":        "manual-review-01",
			"max_correction_ms": 500,
			"decision_source":   "manual",
			"manual_offset_ms":  -90,
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusConflict || errorCode(body) != "conform_conflict" {
		return fmt.Errorf("changed manual retry returned %d: %s", response.StatusCode, body)
	}

	// Reusing the same id across automatic and manual decisions is also rejected.
	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/manual-analysis-01/conforms",
		map[string]any{
			"conform_id":        "manual-review-01",
			"max_correction_ms": 500,
			"decision_source":   "automatic",
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusConflict || errorCode(body) != "conform_conflict" {
		return fmt.Errorf("decision-source switch returned %d: %s", response.StatusCode, body)
	}

	// A manual correction beyond the per-request ceiling fails terminally and
	// never exposes a bundle.
	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/manual-analysis-01/conforms",
		map[string]any{
			"conform_id":        "manual-too-large-01",
			"max_correction_ms": 25,
			"decision_source":   "manual",
			"manual_offset_ms":  200,
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusUnprocessableEntity || errorCode(body) != "correction_limit_exceeded" {
		return fmt.Errorf("oversized manual correction returned %d: %s", response.StatusCode, body)
	}
	response, body, err = ctx.requestJSON(http.MethodGet, "/v1/conforms/manual-too-large-01/bundle", nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusConflict {
		return fmt.Errorf("failed manual conform exposed a bundle: %d %s", response.StatusCode, body)
	}

	// Invalid decision payloads are rejected before any record is created.
	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/manual-analysis-01/conforms",
		map[string]any{
			"conform_id":      "manual-invalid-01",
			"decision_source": "manual",
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusBadRequest || errorCode(body) != "invalid_request" {
		return fmt.Errorf("manual request without offset returned %d: %s", response.StatusCode, body)
	}
	response, body, err = ctx.requestJSON(
		http.MethodPost,
		"/v1/sync-analyses/manual-analysis-01/conforms",
		map[string]any{
			"conform_id":       "manual-invalid-02",
			"decision_source":  "guesswork",
			"manual_offset_ms": 10,
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusBadRequest || errorCode(body) != "invalid_request" {
		return fmt.Errorf("unknown decision source returned %d: %s", response.StatusCode, body)
	}

	// The automatic analysis remains terminal and untouched after every decision.
	response, body, err = ctx.requestJSON(http.MethodGet, "/v1/sync-analyses/manual-analysis-01", nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &analysis); err != nil {
		return fmt.Errorf("decode final analysis: %w", err)
	}
	if analysis.State != domain.AnalysisStateReady || analysis.OffsetMS == nil || *analysis.OffsetMS != 125 {
		return fmt.Errorf("analysis was rewritten by conform decisions")
	}
	return nil
}

func admitTake(ctx *checkContext, id string) error {
	response, body, err := ctx.requestJSON(
		http.MethodPost,
		"/v1/takes",
		takePayload(id, "workflow take", ctx.audio, ctx.video),
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("admit %s returned %d: %s", id, response.StatusCode, body)
	}
	return nil
}

func createAnalysis(ctx *checkContext, takeID, analysisID string) error {
	response, body, err := ctx.requestJSON(
		http.MethodPost,
		"/v1/takes/"+takeID+"/sync-analyses",
		map[string]any{
			"analysis_id":    analysisID,
			"min_confidence": 0.55,
			"max_offset_ms":  1000,
		},
	)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("analysis %s returned %d: %s", analysisID, response.StatusCode, body)
	}
	var analysis domain.SyncAnalysis
	if err := json.Unmarshal(body, &analysis); err != nil {
		return err
	}
	if analysis.State != domain.AnalysisStateReady {
		return fmt.Errorf("analysis %s ended in %s", analysisID, analysis.State)
	}
	return nil
}

func takePayload(id, name string, audio, video []byte) map[string]any {
	return map[string]any{
		"take_id":      id,
		"name":         name,
		"note":         "workflow calibration",
		"audio_base64": base64.StdEncoding.EncodeToString(audio),
		"video_base64": base64.StdEncoding.EncodeToString(video),
	}
}

func (ctx *checkContext) requestJSON(
	method string,
	path string,
	payload any,
) (*http.Response, []byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, ctx.server.URL+path, body)
	if err != nil {
		return nil, nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := ctx.client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, err
	}
	return response, data, nil
}

func readZip(data []byte) (map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open bundle: %w", err)
	}
	entries := make(map[string][]byte)
	for _, entry := range reader.File {
		if strings.Contains(entry.Name, "..") {
			return nil, fmt.Errorf("bundle entry path is unsafe")
		}
		file, err := entry.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			return nil, err
		}
		entries[entry.Name] = content
	}
	return entries, nil
}

func errorCode(body []byte) string {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	return envelope.Error.Code
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
