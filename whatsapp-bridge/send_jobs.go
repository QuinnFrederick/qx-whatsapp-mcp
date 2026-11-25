package main

// send_jobs.go — slow-send jobs (fork addition, see FORK.md).
//
// A send job delivers one text message to each of many people (personal event
// invites, say), spaced out the way a person would send them by hand: a random
// 45–90 s gap between messages and a 5 minute break after every 25. Jobs run in
// the bridge process in the background, independent of any MCP session, and
// live in their own SQLite file (store/send_jobs.db) so a restart loses nothing.
//
// Safety rules, all enforced by the persisted state rather than by memory:
//
//   - A recipient is claimed (pending → sending) in SQLite before its message
//     is sent and marked sent or failed afterwards. A trigger makes "sent"
//     final, so no code path can ever send that recipient again.
//   - A row still "sending" at startup means the bridge stopped mid-send. It
//     becomes sent if messages.db shows the message went out, otherwise failed
//     with an "unknown delivery" error, and its job is paused. Nothing is ever
//     retried automatically.
//   - Any failed send pauses its job. Failed recipients are tried again only
//     when the caller resumes the job with retry_failed.
//   - Jobs run one at a time, oldest first, and share one pacer, so starting a
//     second job never doubles the account's send rate.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	sendJobsDBPath = "store/send_jobs.db"

	// Persisted job statuses.
	jobRunning   = "running"
	jobPaused    = "paused"
	jobCompleted = "completed"
	jobCancelled = "cancelled"
	// jobQueued is never stored: it is how a running job is reported while an
	// older running job is ahead of it.
	jobQueued = "queued"

	recipientPending = "pending"
	recipientSending = "sending"
	recipientSent    = "sent"
	recipientFailed  = "failed"

	maxSendJobNameRunes  = 100
	maxSendJobLabelRunes = 200
	// WhatsApp rejects text messages longer than this many characters.
	maxWhatsAppTextRunes = 65536

	sendJobConnectionRetry = 15 * time.Second
	sendJobIdleRecheck     = 10 * time.Minute
	sendJobDBErrorBackoff  = 30 * time.Second

	// interruptedSendError is recorded for a recipient whose send was cut off
	// by a bridge restart and could not be found in messages.db afterwards.
	interruptedSendError = "the bridge stopped while this message was being sent, so it may or may not have " +
		"been delivered; check the chat before retrying this recipient"
)

// placeholderPattern finds {word} tokens in a template. {name} is the only one
// filled in; any other is rejected so a typo like {Name} or a hoped-for {date}
// is never sent literally to everyone.
var placeholderPattern = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// activeSendJobs is set by startSendJobs before the REST server starts. It is
// nil in tests that build the mux without a job runner, and then the
// /api/send-jobs routes answer 503.
var activeSendJobs *sendJobService

// --- Settings ----------------------------------------------------------------

type sendJobSettings struct {
	MinDelay      time.Duration // shortest gap between two sends
	MaxDelay      time.Duration // longest gap between two sends
	BatchSize     int           // sends between breaks; 0 disables breaks
	BatchBreak    time.Duration // gap after every BatchSize sends
	MaxRecipients int           // per-job ceiling (after de-duplication)
	ForceDryRun   bool          // every new job is created as a dry run
}

func defaultSendJobSettings() sendJobSettings {
	return sendJobSettings{
		MinDelay:      45 * time.Second,
		MaxDelay:      90 * time.Second,
		BatchSize:     25,
		BatchBreak:    5 * time.Minute,
		MaxRecipients: 100,
	}
}

// loadSendJobSettings reads the WHATSAPP_SEND_JOB_* environment variables. A
// value that is not a whole number in range keeps its default and is reported
// in the returned warnings.
func loadSendJobSettings() (sendJobSettings, []string) {
	def := defaultSendJobSettings()
	var warnings []string
	whole := func(key string, min, fallback int) int {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return fallback
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < min {
			warnings = append(warnings, fmt.Sprintf("%s=%q is not a whole number >= %d; using %d", key, raw, min, fallback))
			return fallback
		}
		return n
	}
	s := sendJobSettings{
		MinDelay:      time.Duration(whole("WHATSAPP_SEND_JOB_MIN_DELAY_S", 0, int(def.MinDelay/time.Second))) * time.Second,
		MaxDelay:      time.Duration(whole("WHATSAPP_SEND_JOB_MAX_DELAY_S", 0, int(def.MaxDelay/time.Second))) * time.Second,
		BatchSize:     whole("WHATSAPP_SEND_JOB_BATCH_SIZE", 0, def.BatchSize),
		BatchBreak:    time.Duration(whole("WHATSAPP_SEND_JOB_BATCH_BREAK_S", 0, int(def.BatchBreak/time.Second))) * time.Second,
		MaxRecipients: whole("WHATSAPP_SEND_JOB_MAX_RECIPIENTS", 1, def.MaxRecipients),
		ForceDryRun:   getEnvBool("WHATSAPP_SEND_JOB_DRY_RUN", false),
	}
	if s.MaxDelay < s.MinDelay {
		warnings = append(warnings, fmt.Sprintf("WHATSAPP_SEND_JOB_MAX_DELAY_S (%s) is below the minimum delay; using %s for both",
			s.MaxDelay, s.MinDelay))
		s.MaxDelay = s.MinDelay
	}
	return s, warnings
}

func (s sendJobSettings) json() map[string]any {
	return map[string]any{
		"min_delay_seconds":   int(s.MinDelay / time.Second),
		"max_delay_seconds":   int(s.MaxDelay / time.Second),
		"batch_size":          s.BatchSize,
		"batch_break_seconds": int(s.BatchBreak / time.Second),
		"max_recipients":      s.MaxRecipients,
		"force_dry_run":       s.ForceDryRun,
	}
}

func (s sendJobSettings) randomDelay(r func() float64) time.Duration {
	span := s.MaxDelay - s.MinDelay
	if span <= 0 {
		return s.MinDelay
	}
	return s.MinDelay + time.Duration(r()*float64(span))
}

// --- Pacer -------------------------------------------------------------------

// sendPacer is the account-wide send rhythm, shared by every job and persisted
// so a restart keeps the current gap or break.
type sendPacer struct {
	NextSendAt      time.Time
	LastSendAt      time.Time
	SendsSinceBreak int
	OnBreak         bool // NextSendAt is the end of a batch break
}

// advance returns the pacer after a send attempt that finished at `at`. Every
// attempt counts, failed or not, because WhatsApp saw the traffic either way.
// After every BatchSize-th attempt the gap is BatchBreak instead of a random
// delay. A quiet spell at least as long as a break counts as one.
func (p sendPacer) advance(at time.Time, s sendJobSettings, r func() float64) sendPacer {
	n := p.SendsSinceBreak
	if s.BatchBreak > 0 && !p.LastSendAt.IsZero() && at.Sub(p.LastSendAt) >= s.BatchBreak {
		n = 0
	}
	n++
	if s.BatchSize > 0 && n >= s.BatchSize {
		return sendPacer{NextSendAt: at.Add(s.BatchBreak), LastSendAt: at, OnBreak: true}
	}
	return sendPacer{NextSendAt: at.Add(s.randomDelay(r)), LastSendAt: at, SendsSinceBreak: n}
}

// estimateRemaining is a rough duration for sending `pending` more messages.
func estimateRemaining(pending int, p sendPacer, s sendJobSettings, now time.Time) time.Duration {
	if pending <= 0 {
		return 0
	}
	var total time.Duration
	if p.NextSendAt.After(now) {
		total = p.NextSendAt.Sub(now)
	}
	gaps := pending - 1
	breaks := 0
	if s.BatchSize > 0 {
		breaks = (p.SendsSinceBreak + gaps) / s.BatchSize
		if breaks > gaps {
			breaks = gaps
		}
	}
	avg := (s.MinDelay + s.MaxDelay) / 2
	return total + time.Duration(gaps-breaks)*avg + time.Duration(breaks)*s.BatchBreak
}

// --- Request validation -------------------------------------------------------

type sendJobRecipientInput struct {
	Recipient string `json:"recipient"`
	Name      string `json:"name,omitempty"`
}

type sendJobCreateRequest struct {
	Recipients []sendJobRecipientInput `json:"recipients"`
	Template   string                  `json:"template"`
	Label      string                  `json:"label,omitempty"`
	DryRun     bool                    `json:"dry_run,omitempty"`
}

type preparedRecipient struct {
	JID     types.JID
	Input   string
	Name    string
	Message string
}

type duplicateRecipient struct {
	Input       string `json:"recipient"`
	JID         string `json:"jid"`
	DuplicateOf string `json:"duplicate_of"`
}

// validateTemplate checks the message template and reports whether it uses {name}.
func validateTemplate(template string) (usesName bool, err error) {
	if strings.TrimSpace(template) == "" {
		return false, errors.New("template (the message text) is required")
	}
	var unknown []string
	for _, m := range placeholderPattern.FindAllStringSubmatch(template, -1) {
		if m[1] == "name" {
			usesName = true
			continue
		}
		unknown = append(unknown, m[0])
	}
	if len(unknown) > 0 {
		return false, fmt.Errorf("unknown placeholder(s) %s: only {name} is filled in; write everything else out in full",
			strings.Join(unknown, ", "))
	}
	return usesName, nil
}

// parseSendJobRecipient turns a recipient entry into a person's JID. It accepts
// a JID (…@s.whatsapp.net or …@lid) or a phone number with country code in any
// formatting. Group, broadcast and channel JIDs are rejected: a send job writes
// to people one by one.
func parseSendJobRecipient(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	jid, err := parseJIDOrPhone(raw)
	if err != nil {
		return types.JID{}, err
	}
	if !strings.Contains(raw, "@") {
		if strings.HasPrefix(jid.User, "0") {
			return types.JID{}, fmt.Errorf("phone number %q needs its country code instead of a leading 0 (e.g. 447700900123)", raw)
		}
		if n := len(jid.User); n < 7 || n > 15 {
			return types.JID{}, fmt.Errorf("phone number %q has %d digits; a number with country code has 7 to 15", raw, n)
		}
	}
	if jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer {
		return types.JID{}, fmt.Errorf("%q is not a person (groups, broadcast lists and channels are not supported)", raw)
	}
	if jid.User == "" || strings.Trim(jid.User, "0123456789") != "" {
		return types.JID{}, fmt.Errorf("%q is not a valid person JID (the part before @ must be digits)", raw)
	}
	return jid, nil
}

// prepareSendJob validates a create request and returns the de-duplicated
// recipients with their rendered messages. Every invalid entry is reported, not
// just the first, so the caller can fix the list in one go.
//
// lidToPN maps an @lid JID to the person's phone JID when the bridge knows it,
// so the same person given once by phone and once by LID is sent one message.
func prepareSendJob(ctx context.Context, req sendJobCreateRequest, maxRecipients int,
	lidToPN func(context.Context, types.JID) types.JID) ([]preparedRecipient, []duplicateRecipient, []string) {
	var problems []string
	usesName, err := validateTemplate(req.Template)
	if err != nil {
		problems = append(problems, err.Error())
	}
	if utf8.RuneCountInString(req.Label) > maxSendJobLabelRunes {
		problems = append(problems, fmt.Sprintf("label must be %d characters or fewer", maxSendJobLabelRunes))
	}
	if len(req.Recipients) == 0 {
		problems = append(problems, "at least one recipient is required")
	}

	var out []preparedRecipient
	var dups []duplicateRecipient
	firstInput := map[types.JID]string{}
	for i, in := range req.Recipients {
		where := fmt.Sprintf("recipient %d (%q)", i+1, in.Recipient)
		jid, err := parseSendJobRecipient(in.Recipient)
		if err != nil {
			problems = append(problems, where+": "+err.Error())
			continue
		}
		if jid.Server == types.HiddenUserServer && lidToPN != nil {
			if pn := lidToPN(ctx, jid); !pn.IsEmpty() {
				jid = pn.ToNonAD()
			}
		}
		name := strings.TrimSpace(in.Name)
		if utf8.RuneCountInString(name) > maxSendJobNameRunes {
			problems = append(problems, fmt.Sprintf("%s: name must be %d characters or fewer", where, maxSendJobNameRunes))
			continue
		}
		if prev, dup := firstInput[jid]; dup {
			dups = append(dups, duplicateRecipient{Input: strings.TrimSpace(in.Recipient), JID: jid.String(), DuplicateOf: prev})
			continue
		}
		if usesName && name == "" {
			problems = append(problems, where+": the template uses {name} but this recipient has no name")
			continue
		}
		msg := strings.ReplaceAll(req.Template, "{name}", name)
		if utf8.RuneCountInString(msg) > maxWhatsAppTextRunes {
			problems = append(problems, fmt.Sprintf("%s: the message is longer than WhatsApp's %d character limit", where, maxWhatsAppTextRunes))
			continue
		}
		firstInput[jid] = strings.TrimSpace(in.Recipient)
		out = append(out, preparedRecipient{JID: jid, Input: strings.TrimSpace(in.Recipient), Name: name, Message: msg})
	}
	if len(out) > maxRecipients {
		problems = append(problems, fmt.Sprintf(
			"%d unique recipients is more than the per-job ceiling of %d (WHATSAPP_SEND_JOB_MAX_RECIPIENTS); "+
				"the job was not created", len(out), maxRecipients))
	}
	return out, dups, problems
}

// --- Persistence ---------------------------------------------------------------

const sendJobsSchema = `
CREATE TABLE IF NOT EXISTS send_jobs (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	label        TEXT    NOT NULL DEFAULT '',
	template     TEXT    NOT NULL,
	dry_run      INTEGER NOT NULL DEFAULT 0,
	status       TEXT    NOT NULL,
	pause_reason TEXT    NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL,
	finished_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS send_job_recipients (
	job_id          INTEGER NOT NULL REFERENCES send_jobs(id),
	position        INTEGER NOT NULL,
	jid             TEXT    NOT NULL,
	input           TEXT    NOT NULL,
	name            TEXT    NOT NULL DEFAULT '',
	message         TEXT    NOT NULL,
	status          TEXT    NOT NULL,
	attempts        INTEGER NOT NULL DEFAULT 0,
	last_attempt_at INTEGER NOT NULL DEFAULT 0,
	sent_at         INTEGER NOT NULL DEFAULT 0,
	error           TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (job_id, position),
	UNIQUE (job_id, jid)
);

CREATE INDEX IF NOT EXISTS idx_send_job_recipients_jid ON send_job_recipients(jid);

-- "sent" is final, and "sending" may only become "sent" or "failed". Together
-- with the pending -> sending claim this is what makes a double send
-- impossible, whatever the Go code does.
CREATE TRIGGER IF NOT EXISTS send_job_recipient_status_guard
BEFORE UPDATE OF status ON send_job_recipients
WHEN (OLD.status = 'sent' AND NEW.status <> 'sent')
  OR (OLD.status = 'sending' AND NEW.status NOT IN ('sending', 'sent', 'failed'))
BEGIN
	SELECT RAISE(ABORT, 'send job recipient status change not allowed');
END;

CREATE TABLE IF NOT EXISTS send_pacer (
	id                INTEGER PRIMARY KEY CHECK (id = 1),
	next_send_at      INTEGER NOT NULL DEFAULT 0,
	last_send_at      INTEGER NOT NULL DEFAULT 0,
	sends_since_break INTEGER NOT NULL DEFAULT 0,
	on_break          INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO send_pacer (id) VALUES (1);
`

type sendJobDB struct{ db *sql.DB }

// openSendJobDB opens (and creates) the job store. One connection serialises
// every transaction, which the claim/finish logic relies on.
func openSendJobDB(dsn string) (*sendJobDB, error) {
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(sendJobsSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create send job tables: %w", err)
	}
	return &sendJobDB{db: db}, nil
}

func (d *sendJobDB) Close() error { return d.db.Close() }

func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

type sendJobCounts struct {
	Total, Pending, Sending, Sent, Failed int
}

type sendJob struct {
	ID          int64
	Label       string
	Template    string
	DryRun      bool
	Status      string
	PauseReason string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	FinishedAt  time.Time
	Counts      sendJobCounts
}

type sendJobRecipient struct {
	Position      int
	JID           string
	Input         string
	Name          string
	Message       string
	Status        string
	Attempts      int
	LastAttemptAt time.Time
	SentAt        time.Time
	Error         string
}

var errSendJobNotFound = errors.New("send job not found")

// sendJobConflict is a request that does not fit the job's current state (HTTP 409).
type sendJobConflict struct{ msg string }

func (e sendJobConflict) Error() string { return e.msg }

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

func loadJob(q queryer, id int64) (sendJob, error) {
	var j sendJob
	var dry int
	var created, updated, finished int64
	err := q.QueryRow(`SELECT id, label, template, dry_run, status, pause_reason, created_at, updated_at, finished_at
		FROM send_jobs WHERE id = ?`, id).Scan(&j.ID, &j.Label, &j.Template, &dry, &j.Status, &j.PauseReason,
		&created, &updated, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return j, errSendJobNotFound
	}
	if err != nil {
		return j, err
	}
	j.DryRun = dry != 0
	j.CreatedAt, j.UpdatedAt, j.FinishedAt = fromMillis(created), fromMillis(updated), fromMillis(finished)
	j.Counts, err = loadCounts(q, id)
	return j, err
}

func loadCounts(q queryer, id int64) (sendJobCounts, error) {
	var c sendJobCounts
	rows, err := q.Query(`SELECT status, COUNT(*) FROM send_job_recipients WHERE job_id = ? GROUP BY status`, id)
	if err != nil {
		return c, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return c, err
		}
		c.Total += n
		switch status {
		case recipientPending:
			c.Pending = n
		case recipientSending:
			c.Sending = n
		case recipientSent:
			c.Sent = n
		case recipientFailed:
			c.Failed = n
		}
	}
	return c, rows.Err()
}

// withTx runs fn in a transaction and commits only if it returns nil.
func (d *sendJobDB) withTx(fn func(tx *sql.Tx) error) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (d *sendJobDB) job(id int64) (sendJob, error) { return loadJob(d.db, id) }

func (d *sendJobDB) listJobs(status string, limit int) ([]sendJob, error) {
	query := `SELECT id FROM send_jobs`
	var args []any
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	jobs := make([]sendJob, 0, len(ids))
	for _, id := range ids {
		j, err := d.job(id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (d *sendJobDB) recipients(id int64, status string) ([]sendJobRecipient, error) {
	query := `SELECT position, jid, input, name, message, status, attempts, last_attempt_at, sent_at, error
		FROM send_job_recipients WHERE job_id = ?`
	args := []any{id}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	rows, err := d.db.Query(query+` ORDER BY position`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []sendJobRecipient
	for rows.Next() {
		var r sendJobRecipient
		var last, sent int64
		if err := rows.Scan(&r.Position, &r.JID, &r.Input, &r.Name, &r.Message, &r.Status, &r.Attempts,
			&last, &sent, &r.Error); err != nil {
			return nil, err
		}
		r.LastAttemptAt, r.SentAt = fromMillis(last), fromMillis(sent)
		out = append(out, r)
	}
	return out, rows.Err()
}

// activeJobID is the job the runner works on: the oldest running one.
func (d *sendJobDB) activeJobID() (int64, bool, error) {
	var id int64
	err := d.db.QueryRow(`SELECT id FROM send_jobs WHERE status = ? ORDER BY id LIMIT 1`, jobRunning).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

type sendJobOverlap struct {
	JID   string
	JobID int64
}

// createJob stores a new running job. For a real (not dry-run) job it first
// refuses recipients who are already in another unfinished real job, inside the
// same transaction, because that is how invites get sent twice: a job paused
// after a failure and a fresh job started for the same list.
func (d *sendJobDB) createJob(label, template string, dryRun bool, recips []preparedRecipient, now time.Time) (int64, []sendJobOverlap, error) {
	var id int64
	var overlaps []sendJobOverlap
	err := d.withTx(func(tx *sql.Tx) error {
		if !dryRun {
			rows, err := tx.Query(`SELECT r.jid, r.job_id FROM send_job_recipients r
				JOIN send_jobs j ON j.id = r.job_id
				WHERE j.status IN (?, ?) AND j.dry_run = 0 ORDER BY r.job_id, r.position`, jobRunning, jobPaused)
			if err != nil {
				return err
			}
			wanted := make(map[string]struct{}, len(recips))
			for _, r := range recips {
				wanted[r.JID.String()] = struct{}{}
			}
			for rows.Next() {
				var o sendJobOverlap
				if err := rows.Scan(&o.JID, &o.JobID); err != nil {
					_ = rows.Close()
					return err
				}
				if _, ok := wanted[o.JID]; ok {
					overlaps = append(overlaps, o)
				}
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(overlaps) > 0 {
				return nil
			}
		}
		res, err := tx.Exec(`INSERT INTO send_jobs (label, template, dry_run, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, label, template, boolInt(dryRun), jobRunning, toMillis(now), toMillis(now))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT INTO send_job_recipients (job_id, position, jid, input, name, message, status)
			VALUES (?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for i, r := range recips {
			if _, err := stmt.Exec(id, i+1, r.JID.String(), r.Input, r.Name, r.Message, recipientPending); err != nil {
				return err
			}
		}
		return nil
	})
	return id, overlaps, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (d *sendJobDB) pacer() (sendPacer, error) {
	var next, last int64
	var p sendPacer
	var onBreak int
	err := d.db.QueryRow(`SELECT next_send_at, last_send_at, sends_since_break, on_break FROM send_pacer WHERE id = 1`).
		Scan(&next, &last, &p.SendsSinceBreak, &onBreak)
	p.NextSendAt, p.LastSendAt, p.OnBreak = fromMillis(next), fromMillis(last), onBreak != 0
	return p, err
}

type sendWork struct {
	JobID    int64
	DryRun   bool
	Position int
	JID      string
	Name     string
	Message  string
}

// nextWork finds the next pending recipient of the oldest running job. A
// running job with nothing pending or in flight is marked completed on the way.
func (d *sendJobDB) nextWork(now time.Time) (sendWork, bool, error) {
	rows, err := d.db.Query(`SELECT id, dry_run FROM send_jobs WHERE status = ? ORDER BY id`, jobRunning)
	if err != nil {
		return sendWork{}, false, err
	}
	var jobs []sendWork
	for rows.Next() {
		var w sendWork
		var dry int
		if err := rows.Scan(&w.JobID, &dry); err != nil {
			_ = rows.Close()
			return sendWork{}, false, err
		}
		w.DryRun = dry != 0
		jobs = append(jobs, w)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return sendWork{}, false, err
	}
	for _, w := range jobs {
		err := d.db.QueryRow(`SELECT position, jid, name, message FROM send_job_recipients
			WHERE job_id = ? AND status = ? ORDER BY position LIMIT 1`, w.JobID, recipientPending).
			Scan(&w.Position, &w.JID, &w.Name, &w.Message)
		if err == nil {
			return w, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return sendWork{}, false, err
		}
		if _, err := d.db.Exec(`UPDATE send_jobs SET status = ?, finished_at = ?, updated_at = ? WHERE id = ? AND status = ?
			AND NOT EXISTS (SELECT 1 FROM send_job_recipients WHERE job_id = ? AND status = ?)`,
			jobCompleted, toMillis(now), toMillis(now), w.JobID, jobRunning, w.JobID, recipientSending); err != nil {
			return sendWork{}, false, err
		}
	}
	return sendWork{}, false, nil
}

// claim moves a recipient from pending to sending, but only while its job is
// still running. It is committed before the message goes out, so a crash during
// the send leaves a "sending" row behind instead of a pending one that would be
// sent again.
func (d *sendJobDB) claim(w sendWork, now time.Time) (bool, error) {
	res, err := d.db.Exec(`UPDATE send_job_recipients
		SET status = ?, attempts = attempts + 1, last_attempt_at = ?, error = ''
		WHERE job_id = ? AND position = ? AND status = ?
		  AND EXISTS (SELECT 1 FROM send_jobs WHERE id = ? AND status = ?)`,
		recipientSending, toMillis(now), w.JobID, w.Position, recipientPending, w.JobID, jobRunning)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// finish records the outcome of a claimed send and the new pacer state. A
// failure pauses the job; the last success completes it.
func (d *sendJobDB) finish(w sendWork, sendErr error, at time.Time, p sendPacer) error {
	return d.withTx(func(tx *sql.Tx) error {
		ms := toMillis(at)
		if sendErr == nil {
			if _, err := tx.Exec(`UPDATE send_job_recipients SET status = ?, sent_at = ? WHERE job_id = ? AND position = ? AND status = ?`,
				recipientSent, ms, w.JobID, w.Position, recipientSending); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(`UPDATE send_job_recipients SET status = ?, error = ? WHERE job_id = ? AND position = ? AND status = ?`,
				recipientFailed, sendErr.Error(), w.JobID, w.Position, recipientSending); err != nil {
				return err
			}
			who := w.JID
			if w.Name != "" {
				who = fmt.Sprintf("%s (%s)", w.Name, w.JID)
			}
			reason := fmt.Sprintf("paused automatically: sending to %s failed: %s. Fix the cause, then resume "+
				"(retry_failed=true to try that recipient again).", who, sendErr.Error())
			if _, err := tx.Exec(`UPDATE send_jobs SET status = ?, pause_reason = ?, updated_at = ? WHERE id = ? AND status = ?`,
				jobPaused, reason, ms, w.JobID, jobRunning); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE send_pacer SET next_send_at = ?, last_send_at = ?, sends_since_break = ?, on_break = ? WHERE id = 1`,
			toMillis(p.NextSendAt), toMillis(p.LastSendAt), p.SendsSinceBreak, boolInt(p.OnBreak)); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE send_jobs SET updated_at = ? WHERE id = ?`, ms, w.JobID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE send_jobs SET status = ?, finished_at = ? WHERE id = ? AND status = ?
			AND NOT EXISTS (SELECT 1 FROM send_job_recipients WHERE job_id = ? AND status IN (?, ?))`,
			jobCompleted, ms, w.JobID, jobRunning, w.JobID, recipientPending, recipientSending)
		return err
	})
}

// recoverInterrupted settles recipients left in "sending" by a bridge that
// stopped mid-send. wasSent reports whether messages.db shows the message going
// out (it may be nil). Affected running jobs are paused; nothing is re-sent.
func (d *sendJobDB) recoverInterrupted(now time.Time, wasSent func(jid, text string, since time.Time) (time.Time, bool)) ([]string, error) {
	type stuck struct {
		jobID    int64
		position int
		jid      string
		message  string
		lastAt   int64
	}
	rows, err := d.db.Query(`SELECT job_id, position, jid, message, last_attempt_at FROM send_job_recipients WHERE status = ?`,
		recipientSending)
	if err != nil {
		return nil, err
	}
	var all []stuck
	for rows.Next() {
		var s stuck
		if err := rows.Scan(&s.jobID, &s.position, &s.jid, &s.message, &s.lastAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, s)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var notes []string
	for _, s := range all {
		// Allow for the phone's and the server's clocks running a little behind ours.
		since := fromMillis(s.lastAt).Add(-time.Minute)
		if wasSent != nil {
			if at, ok := wasSent(s.jid, s.message, since); ok {
				if _, err := d.db.Exec(`UPDATE send_job_recipients SET status = ?, sent_at = ? WHERE job_id = ? AND position = ? AND status = ?`,
					recipientSent, toMillis(at), s.jobID, s.position, recipientSending); err != nil {
					return notes, err
				}
				notes = append(notes, fmt.Sprintf("job %d: send to %s was interrupted but is in messages.db; marked sent", s.jobID, s.jid))
				continue
			}
		}
		err := d.withTx(func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE send_job_recipients SET status = ?, error = ? WHERE job_id = ? AND position = ? AND status = ?`,
				recipientFailed, interruptedSendError, s.jobID, s.position, recipientSending); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE send_jobs SET status = ?, pause_reason = ?, updated_at = ? WHERE id = ? AND status = ?`,
				jobPaused, fmt.Sprintf("paused automatically: the bridge restarted while sending to %s, so that message may "+
					"or may not have been delivered. Check that chat, then resume (retry_failed=true only if it did not arrive).", s.jid),
				toMillis(now), s.jobID, jobRunning)
			return err
		})
		if err != nil {
			return notes, err
		}
		notes = append(notes, fmt.Sprintf("job %d: send to %s was interrupted by a restart; marked failed and paused the job", s.jobID, s.jid))
	}
	return notes, nil
}

func (d *sendJobDB) pause(id int64, now time.Time) (sendJob, error) {
	var out sendJob
	err := d.withTx(func(tx *sql.Tx) error {
		j, err := loadJob(tx, id)
		if err != nil {
			return err
		}
		switch j.Status {
		case jobPaused:
			out = j
			return nil
		case jobCompleted, jobCancelled:
			return sendJobConflict{fmt.Sprintf("job %d is %s and cannot be paused", id, j.Status)}
		}
		if _, err := tx.Exec(`UPDATE send_jobs SET status = ?, pause_reason = ?, updated_at = ? WHERE id = ?`,
			jobPaused, "paused by request", toMillis(now), id); err != nil {
			return err
		}
		out, err = loadJob(tx, id)
		return err
	})
	return out, err
}

// resume restarts a paused job (or, with retryFailed, a completed one that has
// failed recipients). retryFailed moves failed recipients back to pending; sent
// recipients are never touched.
func (d *sendJobDB) resume(id int64, retryFailed bool, now time.Time) (sendJob, int, error) {
	var out sendJob
	retried := 0
	err := d.withTx(func(tx *sql.Tx) error {
		j, err := loadJob(tx, id)
		if err != nil {
			return err
		}
		if j.Status == jobCancelled {
			msg := fmt.Sprintf("job %d was cancelled and cannot be resumed; start a new job for anyone still pending", id)
			if j.Counts.Sent > 0 {
				msg += fmt.Sprintf(", leaving out the %d it already sent to", j.Counts.Sent)
			}
			return sendJobConflict{msg}
		}
		if retryFailed {
			res, err := tx.Exec(`UPDATE send_job_recipients SET status = ? WHERE job_id = ? AND status = ?`,
				recipientPending, id, recipientFailed)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			retried = int(n)
		}
		if j.Status == jobCompleted && retried == 0 {
			return sendJobConflict{fmt.Sprintf("job %d is already completed and has no failed recipients to retry", id)}
		}
		var pending int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM send_job_recipients WHERE job_id = ? AND status IN (?, ?)`,
			id, recipientPending, recipientSending).Scan(&pending); err != nil {
			return err
		}
		ms := toMillis(now)
		if pending == 0 {
			_, err = tx.Exec(`UPDATE send_jobs SET status = ?, pause_reason = '', finished_at = ?, updated_at = ? WHERE id = ?`,
				jobCompleted, ms, ms, id)
		} else {
			_, err = tx.Exec(`UPDATE send_jobs SET status = ?, pause_reason = '', finished_at = 0, updated_at = ? WHERE id = ?`,
				jobRunning, ms, id)
		}
		if err != nil {
			return err
		}
		out, err = loadJob(tx, id)
		return err
	})
	return out, retried, err
}

func (d *sendJobDB) cancel(id int64, now time.Time) (sendJob, error) {
	var out sendJob
	err := d.withTx(func(tx *sql.Tx) error {
		j, err := loadJob(tx, id)
		if err != nil {
			return err
		}
		switch j.Status {
		case jobCancelled:
			out = j
			return nil
		case jobCompleted:
			return sendJobConflict{fmt.Sprintf("job %d is already completed", id)}
		}
		ms := toMillis(now)
		if _, err := tx.Exec(`UPDATE send_jobs SET status = ?, pause_reason = '', finished_at = ?, updated_at = ? WHERE id = ?`,
			jobCancelled, ms, ms, id); err != nil {
			return err
		}
		out, err = loadJob(tx, id)
		return err
	})
	return out, err
}

// outgoingTextSince reports when a message with exactly this text, sent by us
// to chatJID at or after since, was stored in messages.db. sendWhatsAppMessage
// stores every successful send, so after a crash this tells "it went out" from
// "unknown".
func (store *MessageStore) outgoingTextSince(chatJID, text string, since time.Time) (time.Time, bool) {
	rows, err := store.db.Query(`SELECT timestamp FROM messages WHERE chat_jid = ? AND is_from_me = 1 AND content = ?`,
		chatJID, text)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw any
		if rows.Scan(&raw) != nil {
			continue
		}
		if ts := anchorTime(raw); !ts.IsZero() && !ts.Before(since) {
			return ts, true
		}
	}
	return time.Time{}, false
}

// --- Runner --------------------------------------------------------------------

type sendJobService struct {
	db       *sendJobDB
	settings sendJobSettings

	// send delivers one text message; in production it is sendWhatsAppMessage.
	send func(ctx context.Context, jid, text string) error
	// connected reports whether WhatsApp is reachable; sends wait while it is not.
	connected func() bool
	// lidToPN resolves an @lid JID to the phone JID (empty when unknown).
	lidToPN func(ctx context.Context, lid types.JID) types.JID
	// wasSent checks messages.db during crash recovery (may be nil).
	wasSent func(jid, text string, since time.Time) (time.Time, bool)
	now     func() time.Time
	rand    func() float64
	logf    func(format string, args ...any)

	wakeCh chan struct{}

	mu           sync.Mutex
	waitingForWA bool   // the runner is holding a send until WhatsApp reconnects
	haltReason   string // set when the worker stopped because it could not record a send

	cancel context.CancelFunc
	done   chan struct{}
}

// haltError stops the worker for good: the outcome of a send could not be
// written to the job store, so carrying on could lose track of it or skip the
// pacing. The recipient stays "sending" and the next bridge start settles it
// without re-sending.
type haltError struct{ err error }

func (e haltError) Error() string { return e.err.Error() }

func newSendJobService(db *sendJobDB, settings sendJobSettings) *sendJobService {
	return &sendJobService{
		db:       db,
		settings: settings,
		send: func(context.Context, string, string) error {
			return errors.New("no sender configured")
		},
		connected: func() bool { return true },
		now:       time.Now,
		rand:      rand.Float64,
		logf:      func(string, ...any) {},
		wakeCh:    make(chan struct{}, 1),
	}
}

// start settles sends interrupted by the last shutdown, then runs the worker.
func (s *sendJobService) start() error {
	notes, err := s.db.recoverInterrupted(s.now(), s.wasSent)
	for _, n := range notes {
		s.logf("%s", n)
	}
	if err != nil {
		return fmt.Errorf("recover interrupted sends: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(ctx)
	return nil
}

// stop asks the worker to exit and waits up to timeout. A send already in
// progress is allowed to finish so its outcome is recorded.
func (s *sendJobService) stop(timeout time.Duration) {
	if s.cancel == nil {
		return
	}
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(timeout):
		s.logf("send job worker did not stop within %s", timeout)
	}
}

func (s *sendJobService) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *sendJobService) run(ctx context.Context) {
	defer close(s.done)
	for ctx.Err() == nil {
		wait, err := s.step(ctx)
		var halt haltError
		if errors.As(err, &halt) {
			s.mu.Lock()
			s.haltReason = halt.Error()
			s.mu.Unlock()
			s.logf("send job worker STOPPED, restart the bridge to recover: %v", err)
			return
		}
		if err != nil {
			s.logf("send job worker: %v", err)
			wait = sendJobDBErrorBackoff
		}
		if wait <= 0 {
			continue
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-s.wakeCh:
		case <-t.C:
		}
		t.Stop()
	}
}

func (s *sendJobService) setWaitingForWA(v bool) {
	s.mu.Lock()
	s.waitingForWA = v
	s.mu.Unlock()
}

func (s *sendJobService) isWaitingForWA() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitingForWA
}

// step sends at most one message and returns how long to wait before the next
// step (0 = go again now). Pause, resume, cancel and new jobs wake it early.
func (s *sendJobService) step(ctx context.Context) (time.Duration, error) {
	now := s.now()
	w, ok, err := s.db.nextWork(now)
	if err != nil || !ok {
		s.setWaitingForWA(false)
		return sendJobIdleRecheck, err
	}
	p, err := s.db.pacer()
	if err != nil {
		return 0, err
	}
	if p.NextSendAt.After(now) {
		return p.NextSendAt.Sub(now), nil
	}
	if !w.DryRun && !s.connected() {
		s.setWaitingForWA(true)
		return sendJobConnectionRetry, nil
	}
	s.setWaitingForWA(false)
	claimed, err := s.db.claim(w, now)
	if err != nil || !claimed {
		return 0, err
	}
	var sendErr error
	if !w.DryRun {
		sendErr = s.send(ctx, w.JID, w.Message)
	}
	at := s.now()
	next := p.advance(at, s.settings, s.rand)
	err = s.db.finish(w, sendErr, at, next)
	for retry := 0; err != nil && retry < 3; retry++ {
		time.Sleep(time.Second)
		err = s.db.finish(w, sendErr, at, next)
	}
	if err != nil {
		return 0, haltError{fmt.Errorf("could not record the send to %s in job %d: %w", w.JID, w.JobID, err)}
	}
	switch {
	case w.DryRun:
		s.logf("job %d: dry run, would have sent message %d to %s", w.JobID, w.Position, w.JID)
	case sendErr != nil:
		s.logf("job %d: send %d to %s failed, job paused: %v", w.JobID, w.Position, w.JID, sendErr)
	default:
		s.logf("job %d: sent message %d to %s", w.JobID, w.Position, w.JID)
	}
	return 0, nil
}

// --- JSON ------------------------------------------------------------------------

func timeJSON(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Local().Format(time.RFC3339)
}

// jobJSON describes a job. activeID is the job the runner is working on;
// another running job is reported as queued behind it.
func (s *sendJobService) jobJSON(j sendJob, activeID int64, p sendPacer, now time.Time) map[string]any {
	status := j.Status
	out := map[string]any{
		"id":               j.ID,
		"label":            j.Label,
		"message_template": j.Template,
		"dry_run":          j.DryRun,
		"created_at":       timeJSON(j.CreatedAt),
		"updated_at":       timeJSON(j.UpdatedAt),
		"finished_at":      timeJSON(j.FinishedAt),
		"counts": map[string]int{
			"total":   j.Counts.Total,
			"pending": j.Counts.Pending,
			"sending": j.Counts.Sending,
			"sent":    j.Counts.Sent,
			"failed":  j.Counts.Failed,
		},
	}
	if j.Status == jobPaused {
		out["pause_reason"] = j.PauseReason
	}
	if j.Status == jobRunning {
		if j.ID != activeID {
			status = jobQueued
			out["queued_behind_job"] = activeID
		} else {
			next := p.NextSendAt
			waiting := "pacing_delay"
			s.mu.Lock()
			halted := s.haltReason
			s.mu.Unlock()
			switch {
			case halted != "":
				waiting = "worker_stopped"
				out["worker_error"] = halted + " (restart the bridge to recover)"
			case j.Counts.Sending > 0:
				waiting, next = "sending_now", now
			case !next.After(now) && s.isWaitingForWA() && !j.DryRun:
				waiting, next = "whatsapp_connection", now
			case !next.After(now):
				waiting, next = "ready", now
			case p.OnBreak:
				waiting = "batch_break"
			}
			out["next_send_at"] = timeJSON(next)
			out["waiting_for"] = waiting
			out["estimated_remaining_seconds"] = int(estimateRemaining(j.Counts.Pending, p, s.settings, now) / time.Second)
		}
	}
	out["status"] = status
	return out
}

func recipientJSON(r sendJobRecipient) map[string]any {
	out := map[string]any{
		"position":        r.Position,
		"recipient":       r.Input,
		"jid":             r.JID,
		"name":            r.Name,
		"status":          r.Status,
		"attempts":        r.Attempts,
		"last_attempt_at": timeJSON(r.LastAttemptAt),
		"sent_at":         timeJSON(r.SentAt),
	}
	if r.Error != "" {
		out["error"] = r.Error
	}
	return out
}

// describe loads a job and renders it, optionally with its recipients.
func (s *sendJobService) describe(id int64, withRecipients bool, recipientStatus string) (map[string]any, error) {
	j, err := s.db.job(id)
	if err != nil {
		return nil, err
	}
	activeID, _, err := s.db.activeJobID()
	if err != nil {
		return nil, err
	}
	p, err := s.db.pacer()
	if err != nil {
		return nil, err
	}
	out := s.jobJSON(j, activeID, p, s.now())
	if withRecipients {
		rs, err := s.db.recipients(id, recipientStatus)
		if err != nil {
			return nil, err
		}
		list := make([]map[string]any, 0, len(rs))
		for _, r := range rs {
			list = append(list, recipientJSON(r))
		}
		out["recipients"] = list
		if len(rs) > 0 {
			out["example_message"] = rs[0].Message
		}
		out["pacing"] = s.settings.json()
	}
	return out, nil
}

// --- HTTP ------------------------------------------------------------------------

func sendJobError(w http.ResponseWriter, err error) {
	var conflict sendJobConflict
	switch {
	case errors.Is(err, errSendJobNotFound):
		jsonErr(w, http.StatusNotFound, err.Error())
	case errors.As(err, &conflict):
		jsonErr(w, http.StatusConflict, conflict.msg)
	default:
		jsonErr(w, http.StatusInternalServerError, "send job store error: "+err.Error())
	}
}

func jobIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		jsonErr(w, http.StatusBadRequest, "job id must be a positive whole number")
		return 0, false
	}
	return id, true
}

var sendJobRecipientStatuses = map[string]bool{recipientPending: true, recipientSending: true, recipientSent: true, recipientFailed: true}
var sendJobStatuses = map[string]bool{jobRunning: true, jobPaused: true, jobCompleted: true, jobCancelled: true}

func (s *sendJobService) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req sendJobCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Label = strings.TrimSpace(req.Label)
	recips, dups, problems := prepareSendJob(r.Context(), req, s.settings.MaxRecipients, s.lidToPN)
	if len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"message": "job not created: " + strings.Join(problems, "; "),
			"errors":  problems,
		})
		return
	}
	dryRun := req.DryRun || s.settings.ForceDryRun
	id, overlaps, err := s.db.createJob(req.Label, req.Template, dryRun, recips, s.now())
	if err != nil {
		sendJobError(w, err)
		return
	}
	if len(overlaps) > 0 {
		jobs := map[int64]bool{}
		var jids []string
		var list []map[string]any
		for _, o := range overlaps {
			jobs[o.JobID] = true
			jids = append(jids, o.JID)
			list = append(list, map[string]any{"jid": o.JID, "job_id": o.JobID})
		}
		var ids []string
		for jobID := range jobs {
			ids = append(ids, strconv.FormatInt(jobID, 10))
		}
		sort.Strings(ids)
		writeJSON(w, http.StatusConflict, map[string]any{
			"success": false,
			"message": fmt.Sprintf("job not created: %d recipient(s) are already in unfinished job(s) %s (%s). "+
				"Resume that job instead, or cancel it first; a second job would message them twice.",
				len(overlaps), strings.Join(ids, ", "), strings.Join(jids, ", ")),
			"overlapping_recipients": list,
		})
		return
	}
	s.wake()
	job, err := s.describe(id, true, "")
	if err != nil {
		sendJobError(w, err)
		return
	}
	msg := fmt.Sprintf("Job %d created with %d recipient(s)", id, len(recips))
	if dryRun {
		msg += " as a DRY RUN: it paces and records progress like a real job but sends nothing to WhatsApp"
	}
	if job["status"] == jobQueued {
		msg += fmt.Sprintf("; it starts after job %v finishes", job["queued_behind_job"])
	}
	resp := map[string]any{"success": true, "message": msg + ".", "job": job}
	if len(dups) > 0 {
		resp["duplicates_removed"] = dups
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *sendJobService) handleList(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !sendJobStatuses[status] {
		jsonErr(w, http.StatusBadRequest, "status must be running, paused, completed or cancelled")
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			jsonErr(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	jobs, err := s.db.listJobs(status, limit)
	if err != nil {
		sendJobError(w, err)
		return
	}
	activeID, _, err := s.db.activeJobID()
	if err != nil {
		sendJobError(w, err)
		return
	}
	p, err := s.db.pacer()
	if err != nil {
		sendJobError(w, err)
		return
	}
	now := s.now()
	list := make([]map[string]any, 0, len(jobs))
	for _, j := range jobs {
		list = append(list, s.jobJSON(j, activeID, p, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "jobs": list, "pacing": s.settings.json()})
}

func (s *sendJobService) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := jobIDParam(w, r)
	if !ok {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("recipient_status"))
	if status != "" && !sendJobRecipientStatuses[status] {
		jsonErr(w, http.StatusBadRequest, "recipient_status must be pending, sending, sent or failed")
		return
	}
	job, err := s.describe(id, true, status)
	if err != nil {
		sendJobError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "job": job})
}

func (s *sendJobService) handleAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := jobIDParam(w, r)
		if !ok {
			return
		}
		now := s.now()
		var err error
		var j sendJob
		msg := ""
		switch action {
		case "pause":
			j, err = s.db.pause(id, now)
			msg = fmt.Sprintf("Job %d paused; %d recipient(s) still pending.", id, j.Counts.Pending)
		case "resume":
			var body struct {
				RetryFailed bool `json:"retry_failed"`
			}
			if derr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); derr != nil && !errors.Is(derr, io.EOF) {
				jsonErr(w, http.StatusBadRequest, "invalid JSON: "+derr.Error())
				return
			}
			var retried int
			j, retried, err = s.db.resume(id, body.RetryFailed, now)
			switch {
			case j.Status == jobCompleted:
				msg = fmt.Sprintf("Job %d had nothing left to send and is now completed.", id)
			default:
				msg = fmt.Sprintf("Job %d resumed with %d recipient(s) pending", id, j.Counts.Pending)
				if retried > 0 {
					msg += fmt.Sprintf(", including %d failed one(s) being retried", retried)
				} else if j.Counts.Failed > 0 {
					msg += fmt.Sprintf("; %d failed recipient(s) are left as failed", j.Counts.Failed)
				}
				msg += "."
			}
		case "cancel":
			j, err = s.db.cancel(id, now)
			msg = fmt.Sprintf("Job %d cancelled; %d recipient(s) were never sent and will not be.", id, j.Counts.Pending)
		}
		if err != nil {
			sendJobError(w, err)
			return
		}
		if j.Counts.Sending > 0 && action != "resume" {
			msg += " A message that was already being sent will still go out."
		}
		s.wake()
		job, err := s.describe(id, false, "")
		if err != nil {
			sendJobError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": msg, "job": job})
	}
}

// registerSendJobRoutes mounts the /api/send-jobs endpoints (see FORK.md). With
// a nil service they all answer 503.
func registerSendJobRoutes(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, svc *sendJobService) {
	guard := func(h func(*sendJobService) http.HandlerFunc) http.HandlerFunc {
		if svc == nil {
			return auth(func(w http.ResponseWriter, _ *http.Request) {
				jsonErr(w, http.StatusServiceUnavailable, "send jobs are not available in this bridge (see the bridge log for why)")
			})
		}
		return auth(h(svc))
	}
	mux.HandleFunc("POST /api/send-jobs", guard(func(s *sendJobService) http.HandlerFunc { return s.handleCreate }))
	mux.HandleFunc("GET /api/send-jobs", guard(func(s *sendJobService) http.HandlerFunc { return s.handleList }))
	mux.HandleFunc("GET /api/send-jobs/{id}", guard(func(s *sendJobService) http.HandlerFunc { return s.handleGet }))
	for _, action := range []string{"pause", "resume", "cancel"} {
		mux.HandleFunc("POST /api/send-jobs/{id}/"+action, guard(func(s *sendJobService) http.HandlerFunc { return s.handleAction(action) }))
	}
}

// startSendJobs opens store/send_jobs.db and starts the worker. It returns a
// stop function to call before disconnecting from WhatsApp. If the store cannot
// be opened, send jobs are disabled (their endpoints answer 503) and the rest
// of the bridge carries on.
func startSendJobs(client *whatsmeow.Client, messageStore *MessageStore, logger waLog.Logger) func() {
	log := logger.Sub("SendJobs")
	settings, warnings := loadSendJobSettings()
	for _, w := range warnings {
		log.Warnf("%s", w)
	}
	db, err := openSendJobDB("file:" + sendJobsDBPath + "?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		log.Errorf("send jobs disabled: %v", err)
		return func() {}
	}
	svc := newSendJobService(db, settings)
	svc.logf = log.Infof
	svc.send = func(_ context.Context, jid, text string) error {
		if ok, msg := sendWhatsAppMessage(client, messageStore, jid, text, "", "", "", "", nil); !ok {
			return errors.New(msg)
		}
		return nil
	}
	svc.connected = func() bool { return client.IsConnected() && client.IsLoggedIn() }
	svc.lidToPN = func(ctx context.Context, lid types.JID) types.JID {
		if client.Store == nil || client.Store.LIDs == nil {
			return types.EmptyJID
		}
		pn, err := client.Store.LIDs.GetPNForLID(ctx, lid)
		if err != nil {
			return types.EmptyJID
		}
		return pn
	}
	svc.wasSent = messageStore.outgoingTextSince
	if err := svc.start(); err != nil {
		log.Errorf("send jobs disabled: %v", err)
		_ = db.Close()
		return func() {}
	}
	activeSendJobs = svc
	log.Infof("send jobs ready (gap %s-%s, %s break every %d sends, max %d recipients per job, force dry run %v)",
		settings.MinDelay, settings.MaxDelay, settings.BatchBreak, settings.BatchSize, settings.MaxRecipients, settings.ForceDryRun)
	return func() {
		svc.stop(10 * time.Second)
		_ = db.Close()
	}
}
