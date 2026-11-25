package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// Every test here uses a fake sender; nothing reaches WhatsApp.

type fakeSender struct {
	mu    sync.Mutex
	calls []string // "jid|text"
	fail  map[string]error
	gate  chan struct{} // when set, each send blocks until it can receive
}

func (f *fakeSender) send(_ context.Context, jid, text string) error {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, jid+"|"+text)
	if err, ok := f.fail[jid]; ok {
		return err
	}
	return nil
}

func (f *fakeSender) setFail(jid string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.fail, jid)
		return
	}
	if f.fail == nil {
		f.fail = map[string]error{}
	}
	f.fail[jid] = err
}

func (f *fakeSender) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func instantSettings() sendJobSettings {
	return sendJobSettings{MaxRecipients: 100}
}

func openTestSendJobDB(t *testing.T, path string) *sendJobDB {
	t.Helper()
	db, err := openSendJobDB("file:" + path + "?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("openSendJobDB: %v", err)
	}
	return db
}

type sendJobHarness struct {
	t      *testing.T
	path   string
	db     *sendJobDB
	svc    *sendJobService
	sender *fakeSender
	mux    *http.ServeMux
	closed bool
}

// newSendJobHarness opens a job store at path (a fresh temp file when empty)
// and, if run is true, starts the worker with a fake sender.
func newSendJobHarness(t *testing.T, path string, settings sendJobSettings, sender *fakeSender, run bool) *sendJobHarness {
	t.Helper()
	if path == "" {
		path = filepath.Join(t.TempDir(), "send_jobs.db")
	}
	if sender == nil {
		sender = &fakeSender{}
	}
	h := &sendJobHarness{t: t, path: path, db: openTestSendJobDB(t, path), sender: sender}
	h.svc = newSendJobService(h.db, settings)
	h.svc.send = sender.send
	h.mux = http.NewServeMux()
	registerSendJobRoutes(h.mux, func(f http.HandlerFunc) http.HandlerFunc { return f }, h.svc)
	if run {
		if err := h.svc.start(); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	t.Cleanup(h.close)
	return h
}

func (h *sendJobHarness) close() {
	if h.closed {
		return
	}
	h.closed = true
	if h.sender.gate != nil {
		close(h.sender.gate) // release any send still blocked on the gate
	}
	h.svc.stop(5 * time.Second)
	_ = h.db.Close()
}

func (h *sendJobHarness) do(method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	var r *http.Request
	if body != nil {
		raw, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, r)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		h.t.Fatalf("%s %s: non-JSON response %d %q", method, path, w.Code, w.Body.String())
	}
	return w.Code, out
}

func (h *sendJobHarness) create(req sendJobCreateRequest) int64 {
	h.t.Helper()
	code, out := h.do(http.MethodPost, "/api/send-jobs", req)
	if code != http.StatusOK {
		h.t.Fatalf("create: %d %v", code, out)
	}
	return int64(out["job"].(map[string]any)["id"].(float64))
}

// waitFor polls until cond holds or fails the test after a few seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *sendJobHarness) job(id int64) sendJob {
	h.t.Helper()
	j, err := h.db.job(id)
	if err != nil {
		h.t.Fatalf("job %d: %v", id, err)
	}
	return j
}

func (h *sendJobHarness) waitStatus(id int64, status string) sendJob {
	h.t.Helper()
	var j sendJob
	waitFor(h.t, fmt.Sprintf("job %d to be %s", id, status), func() bool {
		j = h.job(id)
		return j.Status == status
	})
	return j
}

func (h *sendJobHarness) recipientStatuses(id int64) map[string]string {
	h.t.Helper()
	rs, err := h.db.recipients(id, "")
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rs {
		out[r.JID] = r.Status
	}
	return out
}

func people(n int) []sendJobRecipientInput {
	out := make([]sendJobRecipientInput, n)
	for i := range out {
		out[i] = sendJobRecipientInput{Recipient: fmt.Sprintf("4477009%05d", i+1), Name: fmt.Sprintf("P%d", i+1)}
	}
	return out
}

func pn(user string) string { return user + "@s.whatsapp.net" }

// --- Settings and pacing -------------------------------------------------------

func TestLoadSendJobSettingsDefaults(t *testing.T) {
	for _, k := range []string{"WHATSAPP_SEND_JOB_MIN_DELAY_S", "WHATSAPP_SEND_JOB_MAX_DELAY_S", "WHATSAPP_SEND_JOB_BATCH_SIZE",
		"WHATSAPP_SEND_JOB_BATCH_BREAK_S", "WHATSAPP_SEND_JOB_MAX_RECIPIENTS", "WHATSAPP_SEND_JOB_DRY_RUN"} {
		t.Setenv(k, "")
	}
	s, warnings := loadSendJobSettings()
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	want := sendJobSettings{MinDelay: 45 * time.Second, MaxDelay: 90 * time.Second, BatchSize: 25,
		BatchBreak: 5 * time.Minute, MaxRecipients: 100}
	if s != want {
		t.Fatalf("got %+v, want %+v", s, want)
	}
}

func TestLoadSendJobSettingsOverridesAndBadValues(t *testing.T) {
	t.Setenv("WHATSAPP_SEND_JOB_MIN_DELAY_S", "60")
	t.Setenv("WHATSAPP_SEND_JOB_MAX_DELAY_S", "30") // below min: clamped up
	t.Setenv("WHATSAPP_SEND_JOB_BATCH_SIZE", "ten")
	t.Setenv("WHATSAPP_SEND_JOB_BATCH_BREAK_S", "600")
	t.Setenv("WHATSAPP_SEND_JOB_MAX_RECIPIENTS", "0") // must be >= 1
	t.Setenv("WHATSAPP_SEND_JOB_DRY_RUN", "true")
	s, warnings := loadSendJobSettings()
	if s.MinDelay != time.Minute || s.MaxDelay != time.Minute || s.BatchSize != 25 ||
		s.BatchBreak != 10*time.Minute || s.MaxRecipients != 100 || !s.ForceDryRun {
		t.Fatalf("unexpected settings %+v", s)
	}
	if len(warnings) != 3 {
		t.Fatalf("want 3 warnings (batch size, max recipients, max<min), got %v", warnings)
	}
}

func TestPacerRandomDelayStaysInRange(t *testing.T) {
	s := defaultSendJobSettings()
	start := time.Unix(1_700_000_000, 0)
	for _, r := range []float64{0, 0.5, 0.999999} {
		p := sendPacer{}.advance(start, s, func() float64 { return r })
		gap := p.NextSendAt.Sub(start)
		if gap < 45*time.Second || gap > 90*time.Second || p.OnBreak {
			t.Fatalf("r=%v: gap %s, onBreak %v", r, gap, p.OnBreak)
		}
	}
}

func TestPacerBreaksAfterEveryBatch(t *testing.T) {
	s := defaultSendJobSettings()
	half := func() float64 { return 0.5 }
	at := time.Unix(1_700_000_000, 0)
	var p sendPacer
	breaks := 0
	for i := 1; i <= 60; i++ {
		p = p.advance(at, s, half)
		gap := p.NextSendAt.Sub(at)
		if i%25 == 0 {
			if !p.OnBreak || gap != 5*time.Minute {
				t.Fatalf("send %d: want a 5m break, got %s (onBreak %v)", i, gap, p.OnBreak)
			}
			breaks++
		} else if p.OnBreak || gap != 67500*time.Millisecond {
			t.Fatalf("send %d: want a 67.5s gap, got %s (onBreak %v)", i, gap, p.OnBreak)
		}
		at = p.NextSendAt
	}
	if breaks != 2 {
		t.Fatalf("want 2 breaks in 60 sends, got %d", breaks)
	}
}

func TestPacerQuietSpellCountsAsBreak(t *testing.T) {
	s := defaultSendJobSettings()
	at := time.Unix(1_700_000_000, 0)
	p := sendPacer{LastSendAt: at, SendsSinceBreak: 24}
	// The 25th send would trigger a break, but the account has been quiet for 6 minutes.
	p = p.advance(at.Add(6*time.Minute), s, func() float64 { return 0 })
	if p.OnBreak || p.SendsSinceBreak != 1 {
		t.Fatalf("want counter reset to 1 without a break, got %+v", p)
	}
}

func TestEstimateRemaining(t *testing.T) {
	s := defaultSendJobSettings()
	now := time.Unix(1_700_000_000, 0)
	// 26 sends: 25 gaps, one of which is the 5m break; first send is due now.
	got := estimateRemaining(26, sendPacer{}, s, now)
	want := 24*67500*time.Millisecond + 5*time.Minute
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if estimateRemaining(0, sendPacer{}, s, now) != 0 {
		t.Fatal("nothing pending should estimate 0")
	}
}

// --- Validation ------------------------------------------------------------------

func TestPrepareSendJobDedupesAcrossFormatsAndRendersNames(t *testing.T) {
	lid := types.NewJID("184125298348272", types.HiddenUserServer)
	lidToPN := func(_ context.Context, j types.JID) types.JID {
		if j == lid {
			return types.NewJID("447700900002", types.DefaultUserServer)
		}
		return types.EmptyJID
	}
	req := sendJobCreateRequest{
		Template: "Hi {name}, party on Saturday!",
		Recipients: []sendJobRecipientInput{
			{Recipient: "+44 7700 900001", Name: " Alice "},
			{Recipient: "447700900001@s.whatsapp.net", Name: "Alice again"}, // same as above
			{Recipient: "447700900002", Name: "Bob"},
			{Recipient: "184125298348272@lid", Name: "Bob by LID"}, // same person via the LID map
			{Recipient: "447700900003:12@s.whatsapp.net", Name: "Cleo"},
		},
	}
	recips, dups, problems := prepareSendJob(context.Background(), req, 100, lidToPN)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(recips) != 3 || len(dups) != 2 {
		t.Fatalf("want 3 recipients and 2 duplicates, got %d and %d", len(recips), len(dups))
	}
	if recips[0].JID.String() != pn("447700900001") || recips[0].Message != "Hi Alice, party on Saturday!" {
		t.Fatalf("first recipient %+v", recips[0])
	}
	if recips[2].JID.String() != pn("447700900003") {
		t.Fatalf("device suffix not dropped: %s", recips[2].JID)
	}
	if dups[1].DuplicateOf != "447700900002" || dups[1].JID != pn("447700900002") {
		t.Fatalf("LID duplicate %+v", dups[1])
	}
}

func TestPrepareSendJobRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		req  sendJobCreateRequest
		want string
	}{
		{"empty template", sendJobCreateRequest{Template: "  ", Recipients: people(1)}, "template"},
		{"unknown placeholder", sendJobCreateRequest{Template: "Hi {name}, on {date}", Recipients: people(1)}, "{date}"},
		{"wrong case placeholder", sendJobCreateRequest{Template: "Hi {Name}", Recipients: people(1)}, "{Name}"},
		{"no recipients", sendJobCreateRequest{Template: "Hi"}, "at least one recipient"},
		{"missing name", sendJobCreateRequest{Template: "Hi {name}", Recipients: []sendJobRecipientInput{{Recipient: "447700900001"}}}, "no name"},
		{"group jid", sendJobCreateRequest{Template: "Hi", Recipients: []sendJobRecipientInput{{Recipient: "120363012345678901@g.us"}}}, "not a person"},
		{"broadcast", sendJobCreateRequest{Template: "Hi", Recipients: []sendJobRecipientInput{{Recipient: "status@broadcast"}}}, "not a person"},
		{"leading zero", sendJobCreateRequest{Template: "Hi", Recipients: []sendJobRecipientInput{{Recipient: "07700 900123"}}}, "country code"},
		{"too short", sendJobCreateRequest{Template: "Hi", Recipients: []sendJobRecipientInput{{Recipient: "12345"}}}, "7 to 15"},
		{"letters", sendJobCreateRequest{Template: "Hi", Recipients: []sendJobRecipientInput{{Recipient: "bob"}}}, "phone number or JID"},
		{"over ceiling", sendJobCreateRequest{Template: "Hi", Recipients: people(101)}, "ceiling of 100"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, problems := prepareSendJob(context.Background(), c.req, 100, nil)
			if !strings.Contains(strings.Join(problems, "; "), c.want) {
				t.Fatalf("want a problem mentioning %q, got %v", c.want, problems)
			}
		})
	}
}

func TestPrepareSendJobCountsCeilingAfterDedupe(t *testing.T) {
	recips := people(100)
	recips = append(recips, recips[0], recips[1]) // 102 entries, 100 people
	_, dups, problems := prepareSendJob(context.Background(), sendJobCreateRequest{Template: "Hi {name}", Recipients: recips}, 100, nil)
	if len(problems) != 0 || len(dups) != 2 {
		t.Fatalf("problems %v, dups %d", problems, len(dups))
	}
}

func TestPrepareSendJobReportsEveryBadEntry(t *testing.T) {
	req := sendJobCreateRequest{Template: "Hi", Recipients: []sendJobRecipientInput{
		{Recipient: "bob"}, {Recipient: "447700900001"}, {Recipient: "123@g.us"},
	}}
	_, _, problems := prepareSendJob(context.Background(), req, 100, nil)
	if len(problems) != 2 || !strings.Contains(problems[0], "recipient 1") || !strings.Contains(problems[1], "recipient 3") {
		t.Fatalf("problems %v", problems)
	}
}

// --- Store guarantees ------------------------------------------------------------

func TestSentRecipientCanNeverChangeStatus(t *testing.T) {
	h := newSendJobHarness(t, "", instantSettings(), nil, false)
	recips, _, _ := prepareSendJob(context.Background(), sendJobCreateRequest{Template: "Hi", Recipients: people(1)}, 100, nil)
	id, _, err := h.db.createJob("", "Hi", false, recips, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.db.Exec(`UPDATE send_job_recipients SET status = 'sent' WHERE job_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{recipientPending, recipientSending, recipientFailed} {
		_, err := h.db.db.Exec(`UPDATE send_job_recipients SET status = ? WHERE job_id = ?`, status, id)
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("sent -> %s: want the trigger to refuse, got %v", status, err)
		}
	}
}

func TestSendingRecipientCannotGoBackToPending(t *testing.T) {
	h := newSendJobHarness(t, "", instantSettings(), nil, false)
	recips, _, _ := prepareSendJob(context.Background(), sendJobCreateRequest{Template: "Hi", Recipients: people(1)}, 100, nil)
	id, _, _ := h.db.createJob("", "Hi", false, recips, time.Now())
	if _, err := h.db.db.Exec(`UPDATE send_job_recipients SET status = 'sending' WHERE job_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.db.Exec(`UPDATE send_job_recipients SET status = 'pending' WHERE job_id = ?`, id); err == nil {
		t.Fatal("sending -> pending should be refused")
	}
}

func TestDuplicateJIDInOneJobIsRefusedByTheSchema(t *testing.T) {
	h := newSendJobHarness(t, "", instantSettings(), nil, false)
	jid := types.NewJID("447700900001", types.DefaultUserServer)
	_, _, err := h.db.createJob("", "Hi", false, []preparedRecipient{
		{JID: jid, Input: "a", Message: "Hi"}, {JID: jid, Input: "b", Message: "Hi"},
	}, time.Now())
	if err == nil {
		t.Fatal("want a UNIQUE constraint error")
	}
}

// --- Runner ------------------------------------------------------------------------

func TestSendJobSendsEachRecipientOnceAndCompletes(t *testing.T) {
	h := newSendJobHarness(t, "", instantSettings(), nil, true)
	id := h.create(sendJobCreateRequest{Template: "Hi {name}!", Label: "party", Recipients: people(5)})
	h.waitStatus(id, jobCompleted)

	calls := h.sender.sent()
	if len(calls) != 5 {
		t.Fatalf("want 5 sends, got %v", calls)
	}
	for i, c := range calls {
		want := fmt.Sprintf("%s|Hi P%d!", pn(fmt.Sprintf("4477009%05d", i+1)), i+1)
		if c != want {
			t.Fatalf("send %d: got %q, want %q", i, c, want)
		}
	}
	code, out := h.do(http.MethodGet, fmt.Sprintf("/api/send-jobs/%d", id), nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %v", code, out)
	}
	job := out["job"].(map[string]any)
	counts := job["counts"].(map[string]any)
	if job["status"] != jobCompleted || counts["sent"].(float64) != 5 || counts["pending"].(float64) != 0 {
		t.Fatalf("job %v", job)
	}
	recips := job["recipients"].([]any)
	first := recips[0].(map[string]any)
	if first["status"] != recipientSent || first["sent_at"] == nil || first["attempts"].(float64) != 1 {
		t.Fatalf("recipient %v", first)
	}
	if job["example_message"] != "Hi P1!" {
		t.Fatalf("example_message %v", job["example_message"])
	}
}

func TestSendJobFailurePausesAndResumeSkipsOrRetriesFailed(t *testing.T) {
	sender := &fakeSender{fail: map[string]error{pn("447700900002"): errors.New("Error sending message: server returned 479")}}
	h := newSendJobHarness(t, "", instantSettings(), sender, true)
	id := h.create(sendJobCreateRequest{Template: "Hi {name}", Recipients: people(3)})

	j := h.waitStatus(id, jobPaused)
	if !strings.Contains(j.PauseReason, "479") || !strings.Contains(j.PauseReason, "P2") {
		t.Fatalf("pause reason %q", j.PauseReason)
	}
	if got := len(sender.sent()); got != 2 {
		t.Fatalf("want sends to stop after the failure, got %d sends", got)
	}
	st := h.recipientStatuses(id)
	if st[pn("447700900001")] != recipientSent || st[pn("447700900002")] != recipientFailed || st[pn("447700900003")] != recipientPending {
		t.Fatalf("statuses %v", st)
	}
	_, out := h.do(http.MethodGet, fmt.Sprintf("/api/send-jobs/%d?recipient_status=failed", id), nil)
	failed := out["job"].(map[string]any)["recipients"].([]any)
	if len(failed) != 1 || !strings.Contains(failed[0].(map[string]any)["error"].(string), "479") {
		t.Fatalf("failed recipients %v", failed)
	}

	// Resume without retry: only the pending recipient is sent.
	sender.setFail(pn("447700900002"), nil)
	code, out := h.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/resume", id), nil)
	if code != http.StatusOK {
		t.Fatalf("resume: %d %v", code, out)
	}
	h.waitStatus(id, jobCompleted)
	if calls := sender.sent(); len(calls) != 3 || !strings.HasPrefix(calls[2], pn("447700900003")) {
		t.Fatalf("calls %v", calls)
	}
	if h.recipientStatuses(id)[pn("447700900002")] != recipientFailed {
		t.Fatal("failed recipient should stay failed without retry_failed")
	}

	// Resume with retry_failed: the failed recipient is tried once more; nobody else is.
	code, out = h.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/resume", id), map[string]any{"retry_failed": true})
	if code != http.StatusOK || !strings.Contains(out["message"].(string), "1 failed") {
		t.Fatalf("resume retry: %d %v", code, out)
	}
	h.waitStatus(id, jobCompleted)
	calls := sender.sent()
	if len(calls) != 4 || !strings.HasPrefix(calls[3], pn("447700900002")) {
		t.Fatalf("calls %v", calls)
	}
	rs, _ := h.db.recipients(id, "")
	if rs[1].Attempts != 2 || rs[1].Status != recipientSent || rs[1].Error != "" {
		t.Fatalf("retried recipient %+v", rs[1])
	}

	// Nothing left to retry now.
	code, _ = h.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/resume", id), map[string]any{"retry_failed": true})
	if code != http.StatusConflict {
		t.Fatalf("resume of a clean completed job: want 409, got %d", code)
	}
}

func TestPauseAndCancelStopSending(t *testing.T) {
	sender := &fakeSender{gate: make(chan struct{})}
	h := newSendJobHarness(t, "", instantSettings(), sender, true)
	id := h.create(sendJobCreateRequest{Template: "Hi", Recipients: people(4)})

	// The first send is in flight (blocked on the gate). Pause now.
	waitFor(t, "first send to start", func() bool { return h.job(id).Counts.Sending == 1 })
	code, out := h.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/pause", id), nil)
	if code != http.StatusOK || !strings.Contains(out["message"].(string), "already being sent") {
		t.Fatalf("pause: %d %v", code, out)
	}
	sender.gate <- struct{}{} // let the in-flight send finish
	waitFor(t, "in-flight send to be recorded", func() bool { return h.job(id).Counts.Sent == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := len(sender.sent()); n != 1 {
		t.Fatalf("paused job kept sending: %d sends", n)
	}
	if h.job(id).Status != jobPaused {
		t.Fatal("job should still be paused")
	}

	code, out = h.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/cancel", id), nil)
	if code != http.StatusOK || !strings.Contains(out["message"].(string), "3 recipient(s) were never sent") {
		t.Fatalf("cancel: %d %v", code, out)
	}
	code, _ = h.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/resume", id), nil)
	if code != http.StatusConflict {
		t.Fatalf("resume of a cancelled job: want 409, got %d", code)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(sender.sent()); n != 1 {
		t.Fatalf("cancelled job sent more: %d sends", n)
	}
}

func TestRestartNeverResendsAndSettlesInterruptedSend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "send_jobs.db")
	first := newSendJobHarness(t, path, instantSettings(), &fakeSender{}, true)
	id := first.create(sendJobCreateRequest{Template: "Hi {name}", Recipients: people(4)})
	first.waitStatus(id, jobCompleted)
	first.close()

	// Simulate a crash mid-send in a second job: claim a recipient, then "die".
	db := openTestSendJobDB(t, path)
	recips, _, _ := prepareSendJob(context.Background(), sendJobCreateRequest{Template: "Yo {name}", Recipients: people(3)}, 100, nil)
	id2, _, err := db.createJob("", "Yo {name}", false, recips, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w, ok, err := db.nextWork(time.Now())
	if err != nil || !ok || w.JobID != id2 {
		t.Fatalf("nextWork: %+v %v %v", w, ok, err)
	}
	if claimed, err := db.claim(w, time.Now()); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	_ = db.Close()

	// Restart: the interrupted recipient becomes failed, the job pauses, nothing is sent.
	second := newSendJobHarness(t, path, instantSettings(), &fakeSender{}, true)
	j := second.waitStatus(id2, jobPaused)
	if !strings.Contains(j.PauseReason, "restarted") {
		t.Fatalf("pause reason %q", j.PauseReason)
	}
	rs, _ := second.db.recipients(id2, "")
	if rs[0].Status != recipientFailed || rs[0].Error != interruptedSendError {
		t.Fatalf("interrupted recipient %+v", rs[0])
	}
	time.Sleep(50 * time.Millisecond)
	if calls := second.sender.sent(); len(calls) != 0 {
		t.Fatalf("restart sent messages: %v", calls)
	}
	if st := second.job(id).Status; st != jobCompleted {
		t.Fatalf("finished job changed to %s", st)
	}

	// Resuming continues with pending recipients only; the first job is never touched.
	second.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/resume", id2), nil)
	second.waitStatus(id2, jobCompleted)
	calls := second.sender.sent()
	if len(calls) != 2 || strings.HasPrefix(calls[0], pn("447700900001")) {
		t.Fatalf("calls after resume %v", calls)
	}
}

func TestRecoveryMarksSentWhenMessageStoreHasIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "send_jobs.db")
	db := openTestSendJobDB(t, path)
	recips, _, _ := prepareSendJob(context.Background(), sendJobCreateRequest{Template: "Hi {name}", Recipients: people(2)}, 100, nil)
	id, _, _ := db.createJob("", "Hi {name}", false, recips, time.Now())
	w, _, _ := db.nextWork(time.Now())
	claimedAt := time.Now()
	if ok, err := db.claim(w, claimedAt); !ok || err != nil {
		t.Fatalf("claim %v %v", ok, err)
	}
	_ = db.Close()

	ms := newTestMessageStore(t)
	if err := ms.StoreChat(w.JID, "", claimedAt); err != nil {
		t.Fatal(err)
	}
	if err := ms.StoreMessage("3EB0AAAA", w.JID, "447700000000", w.Message, claimedAt.Add(time.Second), true,
		"", "", "", nil, nil, nil, 0, ""); err != nil {
		t.Fatal(err)
	}

	h := newSendJobHarness(t, path, instantSettings(), &fakeSender{}, false)
	h.svc.wasSent = ms.outgoingTextSince
	if err := h.svc.start(); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(id, jobCompleted)
	rs, _ := h.db.recipients(id, "")
	if rs[0].Status != recipientSent || rs[0].Attempts != 1 {
		t.Fatalf("recovered recipient %+v", rs[0])
	}
	// Only the second recipient was sent by the restarted worker.
	if calls := h.sender.sent(); len(calls) != 1 || !strings.HasPrefix(calls[0], pn("447700900002")) {
		t.Fatalf("calls %v", calls)
	}
}

func TestOutgoingTextSinceIgnoresOlderAndIncomingMessages(t *testing.T) {
	ms := newTestMessageStore(t)
	chat := pn("447700900001")
	now := time.Now()
	_ = ms.StoreChat(chat, "", now)
	_ = ms.StoreMessage("old", chat, "me", "Hi", now.Add(-time.Hour), true, "", "", "", nil, nil, nil, 0, "")
	_ = ms.StoreMessage("in", chat, "them", "Hi", now, false, "", "", "", nil, nil, nil, 0, "")
	if _, ok := ms.outgoingTextSince(chat, "Hi", now.Add(-time.Minute)); ok {
		t.Fatal("matched an old or incoming message")
	}
	_ = ms.StoreMessage("new", chat, "me", "Hi", now, true, "", "", "", nil, nil, nil, 0, "")
	if _, ok := ms.outgoingTextSince(chat, "Hi", now.Add(-time.Minute)); !ok {
		t.Fatal("did not find the outgoing message")
	}
}

func TestPacingHoldsTheNextSend(t *testing.T) {
	s := instantSettings()
	s.MinDelay, s.MaxDelay = time.Hour, time.Hour
	h := newSendJobHarness(t, "", s, nil, true)
	id := h.create(sendJobCreateRequest{Template: "Hi", Recipients: people(3)})
	waitFor(t, "first send", func() bool { return h.job(id).Counts.Sent == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := len(h.sender.sent()); n != 1 {
		t.Fatalf("pacing ignored: %d sends", n)
	}
	_, out := h.do(http.MethodGet, fmt.Sprintf("/api/send-jobs/%d", id), nil)
	job := out["job"].(map[string]any)
	next, err := time.Parse(time.RFC3339, job["next_send_at"].(string))
	if err != nil || job["waiting_for"] != "pacing_delay" || time.Until(next) < 59*time.Minute {
		t.Fatalf("job %v (%v)", job, err)
	}
	if rem := job["estimated_remaining_seconds"].(float64); rem < 7100 || rem > 7200 {
		t.Fatalf("estimated_remaining_seconds %v", rem)
	}

	// A pause/resume does not skip the gap: the pacer is persisted and shared.
	h.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/pause", id), nil)
	h.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/resume", id), nil)
	time.Sleep(50 * time.Millisecond)
	if n := len(h.sender.sent()); n != 1 {
		t.Fatalf("pause/resume skipped the gap: %d sends", n)
	}
}

func TestJobsRunOneAtATimeOldestFirst(t *testing.T) {
	sender := &fakeSender{gate: make(chan struct{})}
	h := newSendJobHarness(t, "", instantSettings(), sender, true)
	a := h.create(sendJobCreateRequest{Template: "A", Recipients: people(2)})
	b := h.create(sendJobCreateRequest{Template: "B", Recipients: []sendJobRecipientInput{{Recipient: "15550000001"}}})

	_, out := h.do(http.MethodGet, fmt.Sprintf("/api/send-jobs/%d", b), nil)
	if job := out["job"].(map[string]any); job["status"] != jobQueued || job["queued_behind_job"].(float64) != float64(a) {
		t.Fatalf("second job %v", job)
	}
	for i := 0; i < 3; i++ {
		sender.gate <- struct{}{}
	}
	h.waitStatus(b, jobCompleted)
	calls := sender.sent()
	if len(calls) != 3 || !strings.HasSuffix(calls[0], "|A") || !strings.HasSuffix(calls[1], "|A") || !strings.HasSuffix(calls[2], "|B") {
		t.Fatalf("order %v", calls)
	}
}

func TestSendsWaitForWhatsAppConnection(t *testing.T) {
	h := newSendJobHarness(t, "", instantSettings(), nil, false)
	var mu sync.Mutex
	online := false
	h.svc.connected = func() bool { mu.Lock(); defer mu.Unlock(); return online }
	if err := h.svc.start(); err != nil {
		t.Fatal(err)
	}
	id := h.create(sendJobCreateRequest{Template: "Hi", Recipients: people(1)})
	waitFor(t, "worker to notice the disconnect", h.svc.isWaitingForWA)
	_, out := h.do(http.MethodGet, fmt.Sprintf("/api/send-jobs/%d", id), nil)
	if job := out["job"].(map[string]any); job["waiting_for"] != "whatsapp_connection" || job["status"] != jobRunning {
		t.Fatalf("job %v", job)
	}
	if j := h.job(id); j.Counts.Pending != 1 || j.Counts.Failed != 0 {
		t.Fatalf("a disconnect must not burn the recipient: %+v", j.Counts)
	}
	mu.Lock()
	online = true
	mu.Unlock()
	h.svc.wake()
	h.waitStatus(id, jobCompleted)
}

func TestDryRunSendsNothing(t *testing.T) {
	h := newSendJobHarness(t, "", instantSettings(), nil, false)
	h.svc.connected = func() bool { return false } // a dry run does not need WhatsApp
	if err := h.svc.start(); err != nil {
		t.Fatal(err)
	}
	code, out := h.do(http.MethodPost, "/api/send-jobs", sendJobCreateRequest{Template: "Hi {name}", DryRun: true, Recipients: people(3)})
	if code != http.StatusOK || !strings.Contains(out["message"].(string), "DRY RUN") {
		t.Fatalf("create: %d %v", code, out)
	}
	id := int64(out["job"].(map[string]any)["id"].(float64))
	j := h.waitStatus(id, jobCompleted)
	if !j.DryRun || j.Counts.Sent != 3 {
		t.Fatalf("job %+v", j)
	}
	if calls := h.sender.sent(); len(calls) != 0 {
		t.Fatalf("dry run sent %v", calls)
	}
}

func TestForceDryRunSettingAppliesToEveryJob(t *testing.T) {
	s := instantSettings()
	s.ForceDryRun = true
	h := newSendJobHarness(t, "", s, nil, true)
	id := h.create(sendJobCreateRequest{Template: "Hi", Recipients: people(2)}) // dry_run not asked for
	j := h.waitStatus(id, jobCompleted)
	if !j.DryRun || len(h.sender.sent()) != 0 {
		t.Fatalf("job %+v, calls %v", j, h.sender.sent())
	}
}

// --- HTTP contract -------------------------------------------------------------------

func TestCreateRefusesOverlapWithUnfinishedRealJob(t *testing.T) {
	sender := &fakeSender{fail: map[string]error{pn("447700900001"): errors.New("boom")}}
	h := newSendJobHarness(t, "", instantSettings(), sender, true)
	first := h.create(sendJobCreateRequest{Template: "Hi", Recipients: people(3)})
	h.waitStatus(first, jobPaused)

	code, out := h.do(http.MethodPost, "/api/send-jobs", sendJobCreateRequest{Template: "Hi", Recipients: people(5)})
	if code != http.StatusConflict || !strings.Contains(out["message"].(string), fmt.Sprintf("job(s) %d", first)) {
		t.Fatalf("overlap: %d %v", code, out)
	}
	if n := len(out["overlapping_recipients"].([]any)); n != 3 {
		t.Fatalf("want 3 overlapping recipients, got %d", n)
	}
	// A dry run may overlap: it sends nothing.
	code, _ = h.do(http.MethodPost, "/api/send-jobs", sendJobCreateRequest{Template: "Hi", DryRun: true, Recipients: people(5)})
	if code != http.StatusOK {
		t.Fatalf("dry run overlap refused: %d", code)
	}
	// Once the first job is cancelled, the same people can be in a new job.
	h.do(http.MethodPost, fmt.Sprintf("/api/send-jobs/%d/cancel", first), nil)
	code, _ = h.do(http.MethodPost, "/api/send-jobs", sendJobCreateRequest{Template: "Hi", Recipients: people(5)})
	if code != http.StatusOK {
		t.Fatalf("create after cancel: %d", code)
	}
}

func TestCreateValidationErrorsAreListed(t *testing.T) {
	h := newSendJobHarness(t, "", instantSettings(), nil, false)
	code, out := h.do(http.MethodPost, "/api/send-jobs", sendJobCreateRequest{Template: "Hi {name}", Recipients: people(101)})
	if code != http.StatusBadRequest || out["success"] != false || !strings.Contains(out["message"].(string), "ceiling of 100") {
		t.Fatalf("over ceiling: %d %v", code, out)
	}
	var count int
	_ = h.db.db.QueryRow(`SELECT COUNT(*) FROM send_jobs`).Scan(&count)
	if count != 0 {
		t.Fatal("a rejected job was stored")
	}
	code, out = h.do(http.MethodPost, "/api/send-jobs", map[string]any{"template": "Hi {name}", "recipients": []map[string]string{
		{"recipient": "bob"}, {"recipient": "447700900001"},
	}})
	if code != http.StatusBadRequest || len(out["errors"].([]any)) != 2 {
		t.Fatalf("errors: %d %v", code, out)
	}
}

func TestCreateReportsDuplicatesRemoved(t *testing.T) {
	h := newSendJobHarness(t, "", instantSettings(), nil, false)
	code, out := h.do(http.MethodPost, "/api/send-jobs", sendJobCreateRequest{Template: "Hi", Recipients: []sendJobRecipientInput{
		{Recipient: "447700900001"}, {Recipient: "+44 7700 900001"},
	}})
	if code != http.StatusOK {
		t.Fatalf("create %d %v", code, out)
	}
	dups := out["duplicates_removed"].([]any)
	if len(dups) != 1 || dups[0].(map[string]any)["duplicate_of"] != "447700900001" {
		t.Fatalf("duplicates %v", dups)
	}
	if total := out["job"].(map[string]any)["counts"].(map[string]any)["total"].(float64); total != 1 {
		t.Fatalf("total %v", total)
	}
}

func TestListAndLookupErrors(t *testing.T) {
	h := newSendJobHarness(t, "", instantSettings(), nil, false)
	h.create(sendJobCreateRequest{Template: "One", Label: "first", Recipients: people(1)})
	h.create(sendJobCreateRequest{Template: "Two", Label: "second", Recipients: []sendJobRecipientInput{{Recipient: "15550000001"}}})

	code, out := h.do(http.MethodGet, "/api/send-jobs", nil)
	jobs := out["jobs"].([]any)
	if code != http.StatusOK || len(jobs) != 2 || jobs[0].(map[string]any)["label"] != "second" {
		t.Fatalf("list %d %v", code, out)
	}
	if _, ok := jobs[0].(map[string]any)["recipients"]; ok {
		t.Fatal("list should not include recipients")
	}
	code, out = h.do(http.MethodGet, "/api/send-jobs?status=paused", nil)
	if code != http.StatusOK || len(out["jobs"].([]any)) != 0 {
		t.Fatalf("filtered list %d %v", code, out)
	}
	for path, want := range map[string]int{
		"/api/send-jobs?status=bogus":              http.StatusBadRequest,
		"/api/send-jobs?limit=0":                   http.StatusBadRequest,
		"/api/send-jobs/999":                       http.StatusNotFound,
		"/api/send-jobs/abc":                       http.StatusBadRequest,
		"/api/send-jobs/1?recipient_status=nope":   http.StatusBadRequest,
		"/api/send-jobs/1?recipient_status=sent":   http.StatusOK,
		"/api/send-jobs/1?recipient_status=failed": http.StatusOK,
	} {
		if code, out := h.do(http.MethodGet, path, nil); code != want {
			t.Errorf("GET %s: want %d, got %d %v", path, want, code, out)
		}
	}
	if code, _ := h.do(http.MethodPost, "/api/send-jobs/999/pause", nil); code != http.StatusNotFound {
		t.Errorf("pause unknown job: %d", code)
	}
}

func TestSendJobRoutesWithoutServiceAnswer503(t *testing.T) {
	mux := http.NewServeMux()
	registerSendJobRoutes(mux, func(f http.HandlerFunc) http.HandlerFunc { return f }, nil)
	for _, rq := range []struct{ method, path string }{
		{http.MethodPost, "/api/send-jobs"}, {http.MethodGet, "/api/send-jobs"}, {http.MethodGet, "/api/send-jobs/1"},
		{http.MethodPost, "/api/send-jobs/1/pause"}, {http.MethodPost, "/api/send-jobs/1/resume"}, {http.MethodPost, "/api/send-jobs/1/cancel"},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(rq.method, rq.path, nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: want 503, got %d", rq.method, rq.path, w.Code)
		}
	}
}

func TestSendJobRoutesRequireTheBridgeToken(t *testing.T) {
	const token = "test-token-0123456789abcdef"
	handler := newRESTMux(newTestClient(&mockLIDStore{}), newTestMessageStore(t), 8080, token, nil)
	for _, auth := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/send-jobs", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("auth %q: want 401, got %d", auth, w.Code)
		}
	}
	// With the token, the route exists (503 here because tests start no runner).
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/send-jobs", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("with token: want 503, got %d", w.Code)
	}
}
